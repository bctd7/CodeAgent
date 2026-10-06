package read

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
)

// ImagePrepareOptions 的零值使用默认限制，负值非法。字节预算均指二进制
// 数据（不包含 base64 的约三分之一膨胀）；输入限制在完整像素解码前检查。
type ImagePrepareOptions struct {
	MaxInputBytes  int
	MaxInputPixels int64
	MaxInputSide   int
	MaxOutputBytes int
	MaxOutputSide  int
	JPEGQuality    int
}

// DefaultImagePrepareOptions 每次返回独立配置：20 MiB 输入、4000 万像素、
// 16384 输入边长、4 MiB 输出、2048 输出边长、JPEG 质量 85。
func DefaultImagePrepareOptions() ImagePrepareOptions {
	return ImagePrepareOptions{20 << 20, 40_000_000, 16384, 4 << 20, 2048, 85}
}

// PreparedImage 不持有调用者的原始字节。尺寸与字节数区分输入与实际输出。
type PreparedImage struct {
	MimeType       string
	Data           string // 标准 base64。
	Width          int
	Height         int
	OriginalWidth  int
	OriginalHeight int
	OriginalBytes  int
	OutputBytes    int
}

// PrepareImageBytes 是字节入口，不修改原数据；文件调用者可自行打开后用 reader 入口。
func PrepareImageBytes(ctx context.Context, data []byte, options ImagePrepareOptions) (PreparedImage, error) {
	return PrepareImage(ctx, bytes.NewReader(data), options)
}

// PrepareImage 有界读取并验证静态 JPEG/PNG，拒绝 APNG 和其他格式，不静默丢帧。
// 不执行外部程序。无需转换时保留原编码；转换时移除元数据，
// 不自动应用 EXIF 方向。透明图片输出 PNG，不透明图片输出 JPEG。
// 输出超预算时逐次缩小（最小 1×1），仍不能满足则报错，不返回超预算结果。
// 取消在读写和像素循环间检查；阻塞 reader 的单次 Read 须由调用者解除阻塞。
func PrepareImage(ctx context.Context, reader io.Reader, options ImagePrepareOptions) (PreparedImage, error) {
	var result PreparedImage
	o, err := normalizeImageOptions(options)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if reader == nil {
		return result, fmt.Errorf("图片 reader 不能为空")
	}
	raw, err := readImageInput(ctx, reader, o.MaxInputBytes)
	if err != nil {
		return result, err
	}
	mime := detectSupportedImageMimeType(raw)
	var config image.Config
	switch mime {
	case "image/png":
		if err = rejectAnimatedPNG(raw); err != nil {
			return result, err
		}
		config, err = png.DecodeConfig(bytes.NewReader(raw))
	case "image/jpeg":
		config, err = jpeg.DecodeConfig(bytes.NewReader(raw))
	default:
		return result, fmt.Errorf("仅支持静态 JPEG/PNG 图片；不支持 GIF/WebP/BMP 或未知格式")
	}
	if err != nil {
		return result, fmt.Errorf("图片头无效: %w", err)
	}
	w, h := config.Width, config.Height
	if w <= 0 || h <= 0 || w > o.MaxInputSide || h > o.MaxInputSide || int64(w) > o.MaxInputPixels/int64(h) {
		return result, fmt.Errorf("图片尺寸 %d×%d 超过边长或像素限制", w, h)
	}
	var decoded image.Image
	r := imageContextReader{ctx, bytes.NewReader(raw)}
	if mime == "image/png" {
		decoded, err = png.Decode(r)
	} else {
		decoded, err = jpeg.Decode(r)
	}
	if err != nil {
		return result, fmt.Errorf("图片解码失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result = PreparedImage{MimeType: mime, Width: w, Height: h, OriginalWidth: w, OriginalHeight: h, OriginalBytes: len(raw)}
	if w <= o.MaxOutputSide && h <= o.MaxOutputSide && len(raw) <= o.MaxOutputBytes {
		result.Data, result.OutputBytes = base64.StdEncoding.EncodeToString(raw), len(raw)
		return result, ctx.Err()
	}
	transparent, err := imageHasTransparency(ctx, decoded)
	if err != nil {
		return PreparedImage{}, err
	}
	result.MimeType = "image/jpeg"
	if transparent {
		result.MimeType = "image/png"
	}
	if w > o.MaxOutputSide || h > o.MaxOutputSide {
		scale := float64(o.MaxOutputSide) / float64(max(w, h))
		w, h = max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	}
	for {
		resized, err := resizePreparedImage(ctx, decoded, w, h)
		if err != nil {
			return PreparedImage{}, err
		}
		output := &imageBudgetWriter{ctx: ctx, remaining: o.MaxOutputBytes}
		if transparent {
			err = png.Encode(output, resized)
		} else {
			err = jpeg.Encode(output, resized, &jpeg.Options{Quality: o.JPEGQuality})
		}
		if err == nil {
			if err = ctx.Err(); err != nil {
				return PreparedImage{}, err
			}
			result.Width, result.Height = w, h
			result.OutputBytes = output.buffer.Len()
			result.Data = base64.StdEncoding.EncodeToString(output.buffer.Bytes())
			return result, nil
		}
		if !errors.Is(err, errImageOutputBudget) {
			return PreparedImage{}, err
		}
		if w == 1 && h == 1 {
			return PreparedImage{}, fmt.Errorf("即使 1×1 图片也超过输出预算 %d 字节", o.MaxOutputBytes)
		}
		w, h = max(1, w*3/4), max(1, h*3/4)
	}
}

func normalizeImageOptions(o ImagePrepareOptions) (ImagePrepareOptions, error) {
	d := DefaultImagePrepareOptions()
	for _, pair := range [][2]*int{{&o.MaxInputBytes, &d.MaxInputBytes}, {&o.MaxInputSide, &d.MaxInputSide}, {&o.MaxOutputBytes, &d.MaxOutputBytes}, {&o.MaxOutputSide, &d.MaxOutputSide}, {&o.JPEGQuality, &d.JPEGQuality}} {
		if *pair[0] < 0 {
			return o, fmt.Errorf("图片限制不能为负数")
		}
		if *pair[0] == 0 {
			*pair[0] = *pair[1]
		}
	}
	if o.MaxInputPixels < 0 || o.JPEGQuality > 100 || o.MaxInputBytes == math.MaxInt {
		return o, fmt.Errorf("图片像素限制、字节限制或 JPEG 质量无效")
	}
	if o.MaxInputPixels == 0 {
		o.MaxInputPixels = d.MaxInputPixels
	}
	return o, nil
}

// PNG 解码器会忽略动画扩展，必须先逐块检查，不能简单匹配像素压缩数据。
func rejectAnimatedPNG(raw []byte) error {
	animated, err := hasPNGAnimation(raw)
	if err != nil {
		return err
	}
	if animated {
		return fmt.Errorf("不支持 APNG 动画图片")
	}
	return nil
}

func hasPNGAnimation(raw []byte) (bool, error) {
	animated := false
	for pos := 8; pos < len(raw); {
		if len(raw)-pos < 12 {
			return false, fmt.Errorf("PNG 数据块不完整")
		}
		n := uint64(binary.BigEndian.Uint32(raw[pos:pos+4])) + 12
		if n > uint64(len(raw)-pos) {
			return false, fmt.Errorf("PNG 数据块长度无效")
		}
		switch string(raw[pos+4 : pos+8]) {
		case "acTL", "fcTL", "fdAT":
			animated = true
		}
		pos += int(n)
	}
	return animated, nil
}

func readImageInput(ctx context.Context, reader io.Reader, maxBytes int) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(imageContextReader{ctx, reader}, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("图片输入超过 %d 字节", maxBytes)
	}
	return raw, nil
}

type imageContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r imageContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

var errImageOutputBudget = errors.New("图片输出超过字节预算")

type imageBudgetWriter struct {
	ctx       context.Context
	remaining int
	buffer    bytes.Buffer
}

func (w *imageBudgetWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > w.remaining {
		return 0, errImageOutputBudget
	}
	w.remaining -= len(p)
	return w.buffer.Write(p)
}

func imageHasTransparency(ctx context.Context, img image.Image) (bool, error) {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		for x := b.Min.X; x < b.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a != 0xffff {
				return true, nil
			}
		}
	}
	return false, nil
}

// 双线性采样使用预乘透明度颜色，避免透明边缘出现黑边；不放大图片。
func resizePreparedImage(ctx context.Context, src image.Image, w, h int) (image.Image, error) {
	b := src.Bounds()
	if w == b.Dx() && h == b.Dy() {
		return src, ctx.Err()
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fy := max(0, (float64(y)+0.5)*float64(b.Dy())/float64(h)-0.5)
		y0 := int(fy)
		for x := 0; x < w; x++ {
			fx := max(0, (float64(x)+0.5)*float64(b.Dx())/float64(w)-0.5)
			x0 := int(fx)
			var channels [4]float64
			for dy := 0; dy < 2; dy++ {
				for dx := 0; dx < 2; dx++ {
					weight := (1 - math.Abs(fx-float64(x0+dx))) * (1 - math.Abs(fy-float64(y0+dy)))
					r, g, blue, a := src.At(b.Min.X+min(x0+dx, b.Dx()-1), b.Min.Y+min(y0+dy, b.Dy()-1)).RGBA()
					for i, v := range [4]uint32{r, g, blue, a} {
						channels[i] += float64(v) * weight
					}
				}
			}
			dst.SetRGBA64(x, y, color.RGBA64{R: uint16(channels[0] + 0.5), G: uint16(channels[1] + 0.5), B: uint16(channels[2] + 0.5), A: uint16(channels[3] + 0.5)})
		}
	}
	return dst, nil
}
