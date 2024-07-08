package codec

import "cosmossdk.io/schema"

// HasSchemaCodec is an interface that all codec's should implement in order
// to properly support indexing. // It is not required by KeyCodec or ValueCodec
// in order to preserve backwards compatibility, but a future version of collections
// may make it required and all codec's should aim to implement it. If it is not
// implemented, fallback defaults will be used for indexing that may be sub-optimal.
//
// Implementations of HasSchemaCodec should test that they are conformant using
// schema.ValidateObjectKey or schema.ValidateObjectValue depending on whether
// the codec is a KeyCodec or ValueCodec respectively.
type HasSchemaCodec[T any] interface {
	// SchemaCodec returns the schema codec for the collections codec.
	SchemaCodec() (SchemaCodec[T], error)
}

// SchemaCodec is a codec that supports converting collection codec values to and
// from schema codec values.
type SchemaCodec[T any] struct {
	// Fields are the schema fields that the codec represents. If this is empty,
	// it will be assumed that this codec represents no value (such as an item key
	// or key set value).
	Fields []schema.Field

	// ToSchemaType converts a codec value of type T to a value corresponding to
	// a schema object key or value (depending on whether this is a key or value
	// codec). The returned value should pass validation with schema.ValidateObjectKey
	// or schema.ValidateObjectValue with the fields specified in Fields.
	// If this function is nil, it will be assumed that T already represents a
	// value that conforms to a schema value without any further conversion.
	ToSchemaType func(T) (any, error)

	// FromSchemaType converts a schema object key or value to T.
	// If this function is nil, it will be assumed that T already represents a
	// value that conforms to a schema value without any further conversion.
	FromSchemaType func(any) (T, error)
}
