package auth

type SigningKeyType string

const (
	SSHSigningKeyType SigningKeyType = "ssh"
	GPGSigningKeyType SigningKeyType = "gpg"
)

type SigningKey struct {
	KeyType    SigningKeyType `json:"keyType"`
	PrivateKey string         `json:"privateKey"`
	PublicKey  string         `json:"publicKey"`
	Passphrase string         `json:"passphrase"`
}
