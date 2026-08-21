package atproto

import "testing"

// TestIsInvalidSwapBody pins the exact strings isInvalidSwapBody must (and
// must not) recognize as a failed compare-and-swap on
// com.atproto.repo.putRecord, per atchess-1c9.112.
//
// The "real PDS body" case below is not invented: it is the verbatim
// response captured from a live PDS instance (alice.pds.test, the
// dual-PDS harness) for a putRecord call carrying a stale "swapRecord"
// value --
//
//	HTTP 400
//	{"error":"InvalidSwap","message":"Record was at bafyreig6ijeuskc2cbikhr74vrnm6f7akvbrgzb3kgustzro2ge4rukwqy"}
//
// captured against a throwaway "app.atchess.swapProbe112" collection and
// cleaned up immediately after. Pinning this exact body as a fixture means
// isInvalidSwapBody's discriminator cannot silently drift from what the
// real server actually sends -- which is precisely how atchess-1c9.112
// went unnoticed for as long as it did: every prior concurrency test
// exercised only in-memory PDS doubles, and one of those doubles
// (internal/web/move_concurrency_test.go's raceMockPDS, before this bead)
// emitted the non-existent error name "InvalidSwapError" instead of the
// real "InvalidSwap".
func TestIsInvalidSwapBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "real PDS body (captured live, atchess-1c9.112)",
			body: `{"error":"InvalidSwap","message":"Record was at bafyreig6ijeuskc2cbikhr74vrnm6f7akvbrgzb3kgustzro2ge4rukwqy"}`,
			want: true,
		},
		{
			name: "raceMockPDS's corrected string, post atchess-1c9.112 (exact match to the real server)",
			body: `{"error":"InvalidSwap","message":"record was concurrently updated"}`,
			want: true,
		},
		{
			name: "raceMockPDS's PRE-atchess-1c9.112 string -- the fictional 'InvalidSwapError' that does not occur on a real PDS, kept here so this discriminator would have caught the drift",
			body: `{"error":"InvalidSwapError","message":"record was concurrently updated"}`,
			// isInvalidSwapBody uses a prefix match, so "InvalidSwapError"
			// (which starts with "InvalidSwap") is still recognized here.
			// The point of this case is not that isInvalidSwapBody rejects
			// it -- it deliberately doesn't, by design -- but that this
			// fixture makes the historical double's drift from the real
			// wire format visible and pinned, rather than silently
			// tolerated only by accident of prefix matching.
			want: true,
		},
		{
			name: "empty body",
			body: "",
			want: false,
		},
		{
			name: "HTML body (e.g. a proxy/edge error page, not a JSON API response)",
			body: "<html><head><title>502 Bad Gateway</title></head><body>502 Bad Gateway</body></html>",
			want: false,
		},
		{
			name: "unrelated structured AT Protocol error",
			body: `{"error":"RecordNotFound","message":"Could not locate record"}`,
			want: false,
		},
		{
			name: "unrelated structured AT Protocol error: generic InvalidRequest",
			body: `{"error":"InvalidRequest","message":"could not parse record"}`,
			want: false,
		},
		{
			name: "valid JSON with no error field at all (genuine server failure, not a CAS conflict)",
			body: `{"message":"internal database error"}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isInvalidSwapBody([]byte(tt.body))
			if got != tt.want {
				t.Errorf("isInvalidSwapBody(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}
