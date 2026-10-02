package retirementrelease

import (
	"errors"

	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/qualification"
)

// ErrHTTPEnvelopeTooLarge distinguishes the HTTP envelope budget from the
// standalone retained-file limits. Oversize inputs cannot be sent by splitting
// them across unbound requests or by raising the city-write global limit.
var ErrHTTPEnvelopeTooLarge = errors.New("retirement HTTP envelope exceeds 1 MiB")

// HTTPEnvelope returns the complete canonical UTF-8 transport body within the
// common city-write budget. Sign the digest of these exact bytes. The larger
// standalone Verify limits do not imply transport support for larger envelopes.
func (r Request) HTTPEnvelope() ([]byte, error) {
	encoded, err := qualification.CanonicalJSON(r)
	if err != nil {
		return nil, err
	}
	if len(encoded) > citywriteauth.MaxHTTPBodyBytes {
		return nil, ErrHTTPEnvelopeTooLarge
	}
	return encoded, nil
}
