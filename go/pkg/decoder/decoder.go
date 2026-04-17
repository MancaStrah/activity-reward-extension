package decoder

// DataKind indicates whether the data represents a message or a result.
type DataKind string

const (
	KindMessage DataKind = "message"
	KindResult  DataKind = "result"
)

// Decoder decodes raw bytes into a structured value.
type Decoder interface {
	Decode(data []byte) (any, error)
}
