// Source: umir-factory reference/durable@26166c8ba197

package rabbitmq

import (
	"errors"
	"fmt"
	"strings"

	amqp "github.com/rabbitmq/amqp091-go"
)

// LoadURL reads the broker URL of a service from <prefix>_AMQP_URL or from the
// file named by <prefix>_AMQP_URL_FILE. Setting both, setting neither or an
// invalid URL is an error. The URL carries credentials: it never appears in an
// error text. getenv is os.Getenv in production.
func LoadURL(getenv func(string) string, prefix string) (BrokerURL, error) {
	prefix = strings.TrimSuffix(prefix, "_")
	valueVar, fileVar := prefix+"_AMQP_URL", prefix+"_AMQP_URL_FILE"
	if prefix == "" {
		valueVar, fileVar = "AMQP_URL", "AMQP_URL_FILE"
	}
	value, file := getenv(valueVar), getenv(fileVar)
	switch {
	case value != "" && file != "":
		return "", fmt.Errorf("%s and %s are both set: use one", valueVar, fileVar)
	case value == "" && file == "":
		return "", fmt.Errorf("%s or %s must be set", valueVar, fileVar)
	}
	name := valueVar
	if file != "" {
		name = fileVar
		raw, err := readSecretFile(file, fileVar)
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(raw)
	}
	if value == "" {
		return "", fmt.Errorf("%s is empty", name)
	}
	if err := validateURL(value); err != nil {
		return "", fmt.Errorf("%s is not a valid amqp or amqps URL", name)
	}
	return BrokerURL(value), nil
}

func validateURL(raw string) error {
	if _, err := amqp.ParseURI(raw); err != nil {
		return errors.New("invalid AMQP URL") // the parser error would quote the URL
	}
	return nil
}
