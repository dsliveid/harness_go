package market

import "encoding/json"

// jsonIndent 缩进序列化。
func jsonIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
