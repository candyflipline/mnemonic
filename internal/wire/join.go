package wire

type JoinRequest struct {
	Key     string `json:"key"`
	Machine string `json:"machine"`
}

type JoinAnswer struct {
	Server    string `json:"server"`
	ServerKey string `json:"server_key"`
	Author    string `json:"author"`
}
