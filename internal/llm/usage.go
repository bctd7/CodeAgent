package llm

// Cost 保存各 token 分项和总费用。
type Cost struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
	Total      float64
}

// Usage 是 Provider 无关的 token 用量。
// Reasoning 是 Output 的子集，因此不会再次计入 Total。
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	Total      int64
	Cost       Cost
}

// CalculateCost 使用每百万 token 单价计算费用。
func CalculateCost(usage Usage, pricing Pricing) Usage {
	const million = 1_000_000
	usage.Cost.Input = float64(usage.Input) * pricing.Input / million
	usage.Cost.Output = float64(usage.Output) * pricing.Output / million
	usage.Cost.CacheRead = float64(usage.CacheRead) * pricing.CacheRead / million
	usage.Cost.CacheWrite = float64(usage.CacheWrite) * pricing.CacheWrite / million
	usage.Cost.Total = usage.Cost.Input + usage.Cost.Output + usage.Cost.CacheRead + usage.Cost.CacheWrite
	return usage
}
