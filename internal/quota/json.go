package quota

import "encoding/json"

// jsonUnmarshal is a tiny indirection so the limiter file stays import-light.
func jsonUnmarshal(raw string, v any) error { return json.Unmarshal([]byte(raw), v) }
