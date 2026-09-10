package responses

import "encoding/json"

// jsonMarshal is a tiny indirection to keep the assembler import list short.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
