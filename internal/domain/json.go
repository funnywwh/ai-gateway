package domain

import "encoding/json"

func jsonUnmarshal(raw string, v any) error { return json.Unmarshal([]byte(raw), v) }
