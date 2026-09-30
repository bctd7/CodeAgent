package main

import (
	"fmt"
	"os"

	"CodeAgent/internal/config"
	"CodeAgent/internal/demo"
	"CodeAgent/internal/provider"
)

const configPath = "config.toml"

func main() {
	reportFile := "docs/Agent验收报告.md"

	// 支持命令行快速直达离线、在线或全套自动化验收
	for _, arg := range os.Args[1:] {
		switch arg {
		case "-offline", "--offline":
			suite := demo.RunOfflineAcceptance(true)
			if err := demo.SaveAcceptanceReport(reportFile, suite, nil, nil); err != nil {
				fmt.Fprintf(os.Stderr, "保存验收报告失败: %v\n", err)
			} else {
				fmt.Printf("\n[验收报告已成功保存至]: %s\n", reportFile)
			}
			return
		case "-online", "--online":
			cfg, err := config.Load(configPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
				os.Exit(1)
			}
			entries := cfg.ModelEntries()
			client := provider.NewClient(demo.NewDiagnosticHTTPClient())
			onlineSuite := demo.RunOnlineAcceptanceSuite(entries, client.Stream, true)
			offlineSuite := demo.RunOfflineAcceptance(false)
			if err := demo.SaveAcceptanceReport(reportFile, offlineSuite, &onlineSuite, nil); err != nil {
				fmt.Fprintf(os.Stderr, "保存验收报告失败: %v\n", err)
			} else {
				fmt.Printf("\n[验收报告已成功保存至]: %s\n", reportFile)
			}
			return
		case "-supplemental", "--supplemental":
			offlineSuite := demo.RunOfflineAcceptance(false)
			cfg, err := config.Load(configPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
				os.Exit(1)
			}
			entries := cfg.ModelEntries()
			client := provider.NewClient(demo.NewDiagnosticHTTPClient())
			suppSuite := demo.RunSupplementalOnlineSuite(entries, client.Stream, true)
			if err := demo.SaveAcceptanceReport(reportFile, offlineSuite, nil, &suppSuite); err != nil {
				fmt.Fprintf(os.Stderr, "保存补充验收报告失败: %v\n", err)
			} else {
				fmt.Printf("\n[补充验收报告已成功保存至]: %s\n", reportFile)
			}
			return
		case "-all", "--all":
			offlineSuite := demo.RunOfflineAcceptance(true)
			cfg, err := config.Load(configPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
				os.Exit(1)
			}
			entries := cfg.ModelEntries()
			client := provider.NewClient(demo.NewDiagnosticHTTPClient())
			onlineSuite := demo.RunOnlineAcceptanceSuite(entries, client.Stream, true)
			if err := demo.SaveAcceptanceReport(reportFile, offlineSuite, &onlineSuite, nil); err != nil {
				fmt.Fprintf(os.Stderr, "保存验收报告失败: %v\n", err)
			} else {
				fmt.Printf("\n[全套验收报告已成功保存至]: %s\n", reportFile)
			}
			return
		}
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		os.Exit(1)
	}

	entries := cfg.ModelEntries()
	client := provider.NewClient(demo.NewDiagnosticHTTPClient())

	if err := demo.Run(entries, client.Stream); err != nil {
		fmt.Fprintf(os.Stderr, "演示运行失败: %v\n", err)
		os.Exit(1)
	}
}
