package decoder

import "encoding/json"

// JSONDecoder decodes JSON-encoded bytes into a value of type T.
type JSONDecoder[T any] struct{}

// NewJSONDecoder creates a JSONDecoder.
func NewJSONDecoder[T any]() *JSONDecoder[T] {
	return &JSONDecoder[T]{}
}

func (d *JSONDecoder[T]) Decode(data []byte) (any, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}
