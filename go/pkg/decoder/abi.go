package decoder

import (
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/flare-foundation/go-flare-common/pkg/tee/structs"
)

// ABIDecoder decodes bytes that hold a single ABI-encoded tuple (Solidity
// abi.encode(SomeStruct(...))) into a value of type T.
type ABIDecoder[T any] struct {
	arg abi.Argument
}

// NewABIDecoder creates an ABIDecoder for the given tuple argument.
func NewABIDecoder[T any](arg abi.Argument) *ABIDecoder[T] {
	return &ABIDecoder[T]{arg: arg}
}

func (d *ABIDecoder[T]) Decode(data []byte) (any, error) {
	return structs.Decode[T](d.arg, data)
}

// FlatABIDecoder decodes bytes that hold flat ABI-encoded arguments (Solidity
// abi.encode(val1, val2, ...)) into a value of type T. Argument names must
// match T's field names (Go-capitalized).
type FlatABIDecoder[T any] struct {
	args abi.Arguments
}

// NewFlatABIDecoder creates a FlatABIDecoder for the given argument list.
func NewFlatABIDecoder[T any](args abi.Arguments) *FlatABIDecoder[T] {
	return &FlatABIDecoder[T]{args: args}
}

func (d *FlatABIDecoder[T]) Decode(data []byte) (any, error) {
	values, err := d.args.Unpack(data)
	if err != nil {
		return nil, err
	}
	var out T
	if err := d.args.Copy(&out, values); err != nil {
		return nil, err
	}
	return out, nil
}
