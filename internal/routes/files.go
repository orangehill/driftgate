package routes

// Multipart is the body type of a route that takes files.
const Multipart = "multipart/form-data"

// FileField is a field of a multipart body that takes a file, with the
// synthetic kind (upload.Kinds) a generated request sends in it.
type FileField struct {
	Field string `json:"field"`
	Kind  string `json:"kind"`
}
