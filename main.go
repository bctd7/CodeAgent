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
