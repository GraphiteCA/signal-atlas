//go:build !windows || !amd64

package gpu

import "errors"

func open() (device, string, error) {
	return nil, "", errors.New("Optional GPU scoring currently supports Windows amd64; CPU fallback is active")
}
