package wire

// JoinRequest is the body of POST /i/{token}: the joining device's public
// key and its machine name.
type JoinRequest struct {
	Key     string `json:"key"`
	Machine string `json:"machine"`
}

// JoinAnswer is what a join gets back: the QUIC address to dial, the key
// the server proves there, and the author the machine pushes as.
type JoinAnswer struct {
	Server    string `json:"server"`
	ServerKey string `json:"server_key"`
	Author    string `json:"author"`
}
