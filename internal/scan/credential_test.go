package scan

import (
	"testing"

	"github.com/orangehill/driftgate/internal/pii"
)

// TestPublicCredentialsOutsideJSON: a token in a form body, a presigned
// link in a page and a token element in XML keep the body out of git as a
// token in JSON does.
func TestPublicCredentialsOutsideJSON(t *testing.T) {
	d, _ := pii.New(nil)
	const gho = "gho_16C7e42F292c6912E7710c838347Ae178B4a"
	for _, c := range [][2]string{
		{"application/x-www-form-urlencoded", "access_token=" + gho + "&token_type=bearer"},
		{"text/html", `<a href="https://b.s3.amazonaws.com/a.pdf?X-Amz-Credential=AKIA%2F20241030&amp;X-Amz-Signature=fe5f80f77d5fa3beca038a248ff027d0445342fe2855ddc963176630326f1024">PDF</a>`},
		{"application/xml", "<OAuth><access_token>" + gho + "</access_token></OAuth>"},
	} {
		findings, unscanned := Public(d, [][2]string{{"content-type", c[0]}}, []byte(c[1]))
		if len(findings) != 1 || findings[0].Kind != pii.Credential || unscanned != "" {
			t.Errorf("%s: findings %+v, unscanned %q", c[0], findings, unscanned)
		}
	}
}
