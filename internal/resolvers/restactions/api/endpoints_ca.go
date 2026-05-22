package api

import (
	"encoding/base64"
	"encoding/pem"
)

// normalizeCAData accepts either single-base64-encoded PEM (the
// correctly-shaped client-go contract) or double-base64-encoded PEM
// (the live-cluster `<user>-clientconfig` Secret shape produced by
// some operators). It returns bytes that plumbing's
// `base64.StdEncoding.DecodeString` + `AppendCertsFromPEM` chain will
// accept.
//
// Detection is shape-keyed (per feedback_no_special_cases): PEM text
// begins with `-----BEGIN`, and the `-` character is not in the
// standard base64 alphabet. So a correctly-shaped single-base64 PEM
// can never further base64-decode into a PEM block at step 5 below;
// only the double-base64 shape progresses past step 4.
//
// Algorithm (per ship-307 design §3):
//  1. empty input → passthrough (no CA configured).
//  2. base64-decode the input; on error → passthrough.
//  3. if pem.Decode succeeds on that result → already-correct
//     single-base64 PEM, passthrough.
//  4. base64-decode the inner bytes; on error → passthrough.
//  5. if pem.Decode succeeds on that → double-base64 confirmed,
//     return the inner single-base64 form.
//  6. otherwise → unknown shape, passthrough.
func normalizeCAData(caData []byte) []byte {
	if len(caData) == 0 {
		return caData
	}
	inner, err := base64.StdEncoding.DecodeString(string(caData))
	if err != nil {
		return caData
	}
	if block, _ := pem.Decode(inner); block != nil {
		return caData
	}
	innerDecoded, err := base64.StdEncoding.DecodeString(string(inner))
	if err != nil {
		return caData
	}
	if block, _ := pem.Decode(innerDecoded); block != nil {
		return inner
	}
	return caData
}
