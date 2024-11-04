package option

type TailscaleOutboundOptions struct {
	StateDirectory string `json:"state_directory,omitempty"`
	AuthKey        string `json:"auth_key,omitempty"`
	ControlURL     string `json:"control_url,omitempty"`
	Ephemeral      bool   `json:"ephemeral,omitempty"`
	Hostname       string `json:"hostname,omitempty"`
}
