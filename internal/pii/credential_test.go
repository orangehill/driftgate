package pii

import (
	"strings"
	"testing"
)

// TestScanResponseCredentials: what a public login, refresh, signed link or
// presigned download returns is a credential the repository must not get;
// identifiers, content hashes, stored file names and ordinary text are not.
func TestScanResponseCredentials(t *testing.T) {
	d, _ := New(nil)
	const jwt = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	found := []string{
		`{"access_token":"` + jwt + `","token_type":"Bearer","expires_in":3600}`,
		`{"data":{"token":"1|xK3pQ9vZ2mT7wR4yL8nB1cF6hJ0sD5aG"}}`, // Sanctum
		`{"refresh_token":"def50200a1b2c3d4e5f60718293a4b5c6d7e8f90"}`,
		`{"csrf_token":"Xa8sd7F6a8sDf7as8d9Fq2w3E4r5T6y7U8i9O0pA"}`,
		`{"api_key":"sk_live_51HxQ2eLkdIwHu7ix4Pq3Rz"}`,
		`{"session_id":"a8f5f167f44f4964e6c998dee827110c"}`,
		`{"url":"https://bucket.s3.amazonaws.com/a.pdf?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIA%2F20241030&X-Amz-Signature=fe5f80f77d5fa3beca038a248ff027d0445342fe2855ddc963176630326f1024"}`,
		`{"download":"/files/7?expires=1730300000&signature=4f2a0b9c3e8d7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b"}`,
		`{"link":"https://app.test/reset?token=8c2f3e1d0a9b8c7d6e5f4a3b2c1d0e9f&email=x"}`,
		`{"value":"Bearer ` + jwt + `"}`,
		`<html><script>window.token = "` + jwt + `"</script></html>`,
		// Outside JSON: a form body (GitHub's OAuth), a presigned link in a
		// page, XML elements and attributes, an OAuth redirect's fragment.
		`access_token=GHO&token_type=bearer&scope=repo`,
		`<p><a href="https://b.s3.amazonaws.com/a.pdf?X-Amz-Algorithm=AWS4-HMAC-SHA256&amp;X-Amz-Credential=AKIA%2F20241030&amp;X-Amz-Signature=fe5f80f77d5fa3beca038a248ff027d0445342fe2855ddc963176630326f1024">PDF</a></p>`,
		`<?xml version="1.0"?><OAuth><access_token>GHO</access_token><token_type>bearer</token_type></OAuth>`,
		`<response><credentials token="GHO" expires="3600"/></response>`,
		`Redirecting to https://app.test/cb#access_token=GHO&token_type=bearer`,
		`{"redirect":"https://app.test/cb#access_token=GHO&token_type=bearer"}`,
		// A setting or a tag: the name says what the value is.
		`[{"name":"api_key","value":"GHO"}]`,
		`{"Tags":[{"Value":"GHO","Key":"DeployToken"}]}`,
	}
	const gho = "gho_16C7e42F292c6912E7710c838347Ae178B4a"
	for _, body := range found {
		body = strings.ReplaceAll(body, "GHO", gho)
		if !hasKind(d.ScanResponse([]byte(body)), Credential) {
			t.Errorf("no credential found in %s", body)
		}
		if hasKind(d.Scan([]byte(body)), Credential) {
			t.Errorf("Scan looks for credentials: %s", body)
		}
	}
	clean := []string{
		`{"id":"550e8400-e29b-41d4-a716-446655440000","sha256":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}`,
		`{"hash":"5d41402abc4b2a76b9719d911017c592","etag":"W/\"5d41402abc4b2a76b9719d911017c592\""}`,
		`{"commit":"6b7f2e7a1c9d8e3f4a5b6c7d8e9f0a1b2c3d4e5f","object_id":"507f1f77bcf86cd799439011"}`,
		`{"avatar":"https://app.test/storage/avatars/Xa8sd7F6a8sDf7as8d9Fq2w3E4r5T6y7U8i9O0pA.jpg"}`,
		`{"next":"/api/items?page=2&per_page=50&sort=created_at","cursor":"eyJpZCI6MTAwfQ"}`,
		`{"token_type":"Bearer","key":"settings.notifications.email","password_reset":"enabled"}`,
		`{"title":"How to rotate your API token without downtime in production"}`,
		`<meta name="csrf-token" content="Xa8sd7F6a8sDf7as8d9Fq2w3E4r5T6y7U8i9O0pA">`,
		`[{"name":"api_key","value":"enabled"},{"name":"title","value":"Xa8sd7F6a8sDf7as8d9Fq2w3E4r5T6y7U8i9O0pA"}]`,
		`[{"name":"api_key"},{"value":"GHO"}]`,
		`{"name":"api_key","meta":{"value":"GHO"}}`,
		`page=2&per_page=50&sort=created_at&hash=9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08`,
		`<style>a{color:#fff}</style><a href="/items?page=2&amp;sort=name">2</a> <a href="#top">top</a>`,
		`<!DOCTYPE html><script nonce="Xa8sd7F6a8sDf7as8d9Fq2w3E4r5T6y7U8i9O0pA" integrity="sha384-oqVuAfXRKap7fdgcCY5uykM6+R9GqQ8K/uxy9rx7HNQlGYl1kPzQho1wx4JwY8wC"></script>`,
		`<?xml version="1.0"?><feed><id>urn:uuid:550e8400-e29b-41d4-a716-446655440000</id><title>How to rotate your API token</title><hash>9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08</hash></feed>`,
		`Is it done? Yes#1, see https://app.test/docs?page=2#tokens`,
	}
	for _, body := range clean {
		body = strings.ReplaceAll(body, "GHO", gho)
		if fs := d.ScanResponse([]byte(body)); hasKind(fs, Credential) {
			t.Errorf("credential in %s: %+v", body, fs)
		}
	}
}

func hasKind(fs []Finding, kind string) bool {
	for _, f := range fs {
		if f.Kind == kind && !strings.Contains(f.Sample, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9") {
			return true
		}
	}
	return false
}
