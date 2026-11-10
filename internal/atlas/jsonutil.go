package atlas

import (
	"bytes"
	"encoding/json"
	"errors"
)

// orderedObject decodes a JSON object keeping its keys in document order (Python dicts keep it).
// isObj is false for any other JSON value.
func orderedObject(text string) (keys []string, vals []any, isObj bool, err error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		var rest any
		if !ok {
			return nil, nil, false, nil
		}
		dec2 := json.NewDecoder(bytes.NewReader([]byte(text)))
		if err := dec2.Decode(&rest); err != nil {
			return nil, nil, false, err
		}
		return nil, nil, false, nil
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, false, err
		}
		k, ok := kt.(string)
		if !ok {
			return nil, nil, false, errors.New("object key is not a string")
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, nil, false, err
		}
		// a repeated key keeps its first position and its last value, as a Python dict does
		dup := false
		for i, prev := range keys {
			if prev == k {
				vals[i], dup = v, true
				break
			}
		}
		if !dup {
			keys, vals = append(keys, k), append(vals, v)
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, nil, false, err
	}
	return keys, vals, true, nil
}

func validBasemap(payload []byte) bool {
	var v struct {
		Type     string `json:"type"`
		Features []any  `json:"features"`
	}
	return json.Unmarshal(payload, &v) == nil && v.Type == "FeatureCollection" && v.Features != nil
}

func compactJSON(payload []byte) string {
	var b bytes.Buffer
	if json.Compact(&b, payload) != nil {
		return string(payload)
	}
	return b.String()
}
