package read

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"unicode/utf8"

	"CodeAgent/internal/agent"
	"CodeAgent/internal/llm"
	"CodeAgent/internal/tools/basic/common"
)

const (
	readToolName    = "read"
	maxSafeIntParam = math.MaxInt32
)

var readSchema = []byte(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to the file to read (relative or absolute)"
    },
    "offset": {
      "type": "integer",
      "description": "Line number to start reading from (1-indexed)"
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of lines to read"
    }
  },
  "required": ["path"]
}`)

// New 构造 read 工具实例。
func New(workingDir string) agent.Tool {
	return agent.Tool{
		Tool: llm.Tool{
			Name: readToolName,
			Description: "Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). " +
				"Images are limited to 20MiB input. Static JPEG/PNG images are validated and resized to at most 2048 pixels per side and 4MiB output; GIF/WebP/BMP/APNG are attached unchanged without resizing or pixel limits. For text files, output is truncated to 2000 lines or 50KB (whichever is hit first). " +
				"Use offset/limit for large files. When you need the full file, continue with offset until complete.",
			Schema: readSchema,
		},
		ParallelSafe: true,
		Execute: func(ctx context.Context, callID string, args map[string]any, onProgress func(llm.ContentBlock)) ([]llm.ContentBlock, error) {
			return executeRead(ctx, workingDir, args)
		},
	}
}

func executeRead(ctx context.Context, workingDir string, args map[string]any) ([]llm.ContentBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pathStr, offset, limit, err := parseReadArgs(args)
	if err != nil {
		return nil, err
	}

	resolvedPath, err := common.ResolvePath(workingDir, pathStr)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fi, err := os.Stat(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("无法访问文件 %s: %w", pathStr, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("目标路径为目录而非文件: %s", pathStr)
	}

	file, err := os.Open(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败 %s: %w", pathStr, err)
	}
	defer file.Close()
	var header [18]byte
	n, err := io.ReadFull(imageContextReader{ctx, file}, header[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("读取文件失败 %s: %w", pathStr, err)
	}
	reader := io.MultiReader(bytes.NewReader(header[:n]), file)
	mimeType := detectSupportedImageMimeType(header[:n])
	if mimeType == "" && isBMPWithLength(header[:n], uint64(fi.Size())) {
		mimeType = "image/bmp"
	}
	if mimeType != "" {
		prepared, err := prepareReadImage(ctx, reader, mimeType)
		if err != nil {
			return nil, fmt.Errorf("读取图片失败 %s: %w", pathStr, err)
		}
		return []llm.ContentBlock{
			{Type: llm.TypeText, Text: fmt.Sprintf("Read image file [%s]", prepared.MimeType)},
			{Type: llm.TypeImage, Data: prepared.Data, MimeType: prepared.MimeType},
		}, nil
	}
	rawBytes, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败 %s: %w", pathStr, err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !utf8.Valid(rawBytes) {
		return nil, fmt.Errorf("文件不是合法的 UTF-8 文本或支持的图片格式: %s", pathStr)
	}

	if len(rawBytes) == 0 {
		if offset == 1 {
			return []llm.ContentBlock{{Type: llm.TypeText, Text: ""}}, nil
		}
		return nil, fmt.Errorf("offset %d 超出文件实际范围（文件为空）: %s", offset, pathStr)
	}

	textContent := string(rawBytes)
	if strings.HasPrefix(textContent, "\ufeff") {
		textContent = textContent[len("\ufeff"):]
	}

	allLines := common.SplitLinesForCounting(textContent)
	totalFileLines := len(allLines)
	if totalFileLines == 0 {
		return []llm.ContentBlock{{Type: llm.TypeText, Text: ""}}, nil
	}

	startLine := offset - 1
	startLineDisplay := offset
	if startLine >= totalFileLines {
		return nil, fmt.Errorf("offset %d 超出文件实际行数范围（文件共有 %d 行）: %s", offset, totalFileLines, pathStr)
	}

	var selectedContent string
	var userLimitedLines *int
	if limit != nil {
		endLine := totalFileLines
		if *limit < totalFileLines-startLine {
			endLine = startLine + *limit
		}
		selectedContent = strings.Join(allLines[startLine:endLine], "\n")
		cnt := endLine - startLine
		userLimitedLines = &cnt
	} else {
		selectedContent = strings.Join(allLines[startLine:], "\n")
	}

	truncation := truncateHead(selectedContent, common.TruncateOptions{})
	var outputText string
	if truncation.FirstLineExceedsLimit {
		firstLineSize := common.FormatSize(len(allLines[startLine]))
		if startLineDisplay < totalFileLines {
			outputText = fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use offset=%d or a shell command to inspect.]",
				startLineDisplay, firstLineSize, common.FormatSize(common.DefaultMaxBytes), startLineDisplay+1)
		} else {
			outputText = fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use a shell command to inspect.]",
				startLineDisplay, firstLineSize, common.FormatSize(common.DefaultMaxBytes))
		}
	} else if truncation.Truncated {
		endLineDisplay := startLineDisplay + truncation.OutputLines - 1
		nextOffset := endLineDisplay + 1
		outputText = truncation.Content
		if truncation.TruncatedBy == "lines" {
			outputText += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]",
				startLineDisplay, endLineDisplay, totalFileLines, nextOffset)
		} else {
			outputText += fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Use offset=%d to continue.]",
				startLineDisplay, endLineDisplay, totalFileLines, common.FormatSize(common.DefaultMaxBytes), nextOffset)
		}
	} else if userLimitedLines != nil && startLine+*userLimitedLines < totalFileLines {
		remaining := totalFileLines - (startLine + *userLimitedLines)
		nextOffset := startLine + *userLimitedLines + 1
		outputText = fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]",
			truncation.Content, remaining, nextOffset)
	} else {
		outputText = truncation.Content
	}

	return []llm.ContentBlock{
		{
			Type: llm.TypeText,
			Text: outputText,
		},
	}, nil
}

// 原样格式只限制输入字节；不解码、不缩放，也不保证像素预算。
func prepareReadImage(ctx context.Context, reader io.Reader, mimeType string) (PreparedImage, error) {
	options := DefaultImagePrepareOptions()
	if mimeType == "image/jpeg" {
		return PrepareImage(ctx, reader, options)
	}
	raw, err := readImageInput(ctx, reader, options.MaxInputBytes)
	if err != nil {
		return PreparedImage{}, err
	}
	if mimeType == "image/png" {
		animated, err := hasPNGAnimation(raw)
		if err != nil {
			return PreparedImage{}, err
		}
		if !animated {
			return PrepareImage(ctx, bytes.NewReader(raw), options)
		}
	}
	if detectSupportedImageMimeType(raw) != mimeType {
		return PreparedImage{}, fmt.Errorf("图片格式无效")
	}
	if err := ctx.Err(); err != nil {
		return PreparedImage{}, err
	}
	return PreparedImage{MimeType: mimeType, Data: base64.StdEncoding.EncodeToString(raw), OriginalBytes: len(raw), OutputBytes: len(raw)}, nil
}

func parseReadArgs(args map[string]any) (string, int, *int, error) {
	pathVal, exists := args["path"]
	if !exists {
		return "", 0, nil, errors.New("缺少必填参数 'path'")
	}
	pathStr, ok := pathVal.(string)
	if !ok || strings.TrimSpace(pathStr) == "" {
		return "", 0, nil, errors.New("参数 'path' 必须为非空字符串")
	}

	offset := 1
	if val, exists := args["offset"]; exists && val != nil {
		switch v := val.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
				return "", 0, nil, errors.New("参数 'offset' 必须为整数")
			}
			if v < 1 {
				return "", 0, nil, errors.New("参数 'offset' 必须大于等于 1")
			}
			if v > float64(maxSafeIntParam) {
				return "", 0, nil, errors.New("参数 'offset' 超出支持范围")
			}
			offset = int(v)
		case int:
			if v < 1 {
				return "", 0, nil, errors.New("参数 'offset' 必须大于等于 1")
			}
			offset = v
		case int64:
			if v < 1 {
				return "", 0, nil, errors.New("参数 'offset' 必须大于等于 1")
			}
			if v > int64(maxSafeIntParam) {
				return "", 0, nil, errors.New("参数 'offset' 超出支持范围")
			}
			offset = int(v)
		default:
			return "", 0, nil, errors.New("参数 'offset' 必须为整数")
		}
	}

	var limit *int
	if val, exists := args["limit"]; exists && val != nil {
		switch v := val.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
				return "", 0, nil, errors.New("参数 'limit' 必须为整数")
			}
			if v <= 0 {
				return "", 0, nil, errors.New("参数 'limit' 必须为正整数")
			}
			if v > float64(maxSafeIntParam) {
				return "", 0, nil, errors.New("参数 'limit' 超出支持范围")
			}
			l := int(v)
			limit = &l
		case int:
			if v <= 0 {
				return "", 0, nil, errors.New("参数 'limit' 必须为正整数")
			}
			limit = &v
		case int64:
			if v <= 0 {
				return "", 0, nil, errors.New("参数 'limit' 必须为正整数")
			}
			if v > int64(maxSafeIntParam) {
				return "", 0, nil, errors.New("参数 'limit' 超出支持范围")
			}
			l := int(v)
			limit = &l
		default:
			return "", 0, nil, errors.New("参数 'limit' 必须为整数")
		}
	}

	return pathStr, offset, limit, nil
}

func detectSupportedImageMimeType(buf []byte) string {
	if len(buf) >= 3 && buf[0] == 0xFF && buf[1] == 0xD8 && buf[2] == 0xFF {
		if len(buf) > 3 && buf[3] == 0xF7 {
			return ""
		}
		return "image/jpeg"
	}
	if len(buf) >= 8 &&
		buf[0] == 0x89 && buf[1] == 0x50 && buf[2] == 0x4E && buf[3] == 0x47 &&
		buf[4] == 0x0D && buf[5] == 0x0A && buf[6] == 0x1A && buf[7] == 0x0A {
		return "image/png"
	}
	if len(buf) >= 6 {
		prefix := string(buf[:6])
		if prefix == "GIF87a" || prefix == "GIF89a" {
			return "image/gif"
		}
	}
	if len(buf) >= 12 &&
		string(buf[0:4]) == "RIFF" &&
		string(buf[8:12]) == "WEBP" {
		return "image/webp"
	}
	if isBMP(buf) {
		return "image/bmp"
	}
	return ""
}

func isBMP(buf []byte) bool {
	return isBMPWithLength(buf, uint64(len(buf)))
}

func isBMPWithLength(buf []byte, fileLength uint64) bool {
	if len(buf) < 18 {
		return false
	}
	if buf[0] != 0x42 || buf[1] != 0x4D {
		return false
	}
	// 保留字段必须为 0
	if buf[6] != 0 || buf[7] != 0 || buf[8] != 0 || buf[9] != 0 {
		return false
	}
	bfSize := binary.LittleEndian.Uint32(buf[2:6])
	bfOffBits := binary.LittleEndian.Uint32(buf[10:14])
	biSize := binary.LittleEndian.Uint32(buf[14:18])

	// DIB 信息头常见尺寸：12, 40, 52, 56, 64, 108, 124
	validHeaderSize := biSize == 40 || biSize == 12 || biSize == 108 || biSize == 124 || biSize == 52 || biSize == 56 || biSize == 64
	if !validHeaderSize {
		return false
	}
	if uint64(14)+uint64(biSize) > fileLength {
		return false
	}
	if uint64(bfOffBits) < uint64(14)+uint64(biSize) || uint64(bfOffBits) >= fileLength {
		return false
	}
	if bfSize <= bfOffBits || uint64(bfSize) > fileLength {
		return false
	}
	return true
}

// truncateHead 保留文本开头的完整行，适用于文件读取等从头部查看的场景。
// 统一默认文本上限为 2000 行、50 KiB，先达到者生效。
// 首行单独超过字节上限时，返回空内容并置 FirstLineExceedsLimit 为 true。
func truncateHead(content string, opts common.TruncateOptions) common.TruncateResult {
	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = common.DefaultMaxLines
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = common.DefaultMaxBytes
	}

	totalBytes := len(content)
	lines := common.SplitLinesForCounting(content)
	totalLines := len(lines)

	if totalLines <= maxLines && totalBytes <= maxBytes {
		return common.TruncateResult{
			Content:     content,
			Truncated:   false,
			TruncatedBy: "",
			TotalLines:  totalLines,
			TotalBytes:  totalBytes,
			OutputLines: totalLines,
			OutputBytes: totalBytes,
		}
	}

	if len(lines) > 0 && len(lines[0]) > maxBytes {
		return common.TruncateResult{
			Content:               "",
			Truncated:             true,
			TruncatedBy:           "bytes",
			TotalLines:            totalLines,
			TotalBytes:            totalBytes,
			OutputLines:           0,
			OutputBytes:           0,
			FirstLineExceedsLimit: true,
		}
	}

	var outputLinesArr []string
	outputBytesCount := 0
	truncatedBy := "lines"

	for i := 0; i < len(lines) && i < maxLines; i++ {
		line := lines[i]
		lineBytes := len(line)
		if i > 0 {
			lineBytes++
		}

		if outputBytesCount+lineBytes > maxBytes {
			truncatedBy = "bytes"
			break
		}

		outputLinesArr = append(outputLinesArr, line)
		outputBytesCount += lineBytes
	}

	if len(outputLinesArr) >= maxLines && outputBytesCount <= maxBytes {
		truncatedBy = "lines"
	}

	outputContent := strings.Join(outputLinesArr, "\n")
	return common.TruncateResult{
		Content:     outputContent,
		Truncated:   true,
		TruncatedBy: truncatedBy,
		TotalLines:  totalLines,
		TotalBytes:  totalBytes,
		OutputLines: len(outputLinesArr),
		OutputBytes: len(outputContent),
	}
}
