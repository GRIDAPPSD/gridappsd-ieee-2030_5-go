package sep2embed

import (
	"context"
	"testing"
)

func TestIdentityFromContextRoundTrips(t *testing.T) {
	t.Parallel()

	id := deviceIdentity{lfdi: "AABBCC", sfdi: "112233445566"}
	ctx := context.WithValue(context.Background(), identityCtxKey{}, id)

	lfdi, sfdi, ok := identityFromContext(ctx)
	if !ok {
		t.Fatal("identityFromContext: ok = false, want true")
	}
	if lfdi != id.lfdi {
		t.Errorf("lfdi = %q, want %q", lfdi, id.lfdi)
	}
	if sfdi != id.sfdi {
		t.Errorf("sfdi = %q, want %q", sfdi, id.sfdi)
	}
}

func TestIdentityFromContextMissingReturnsNotOK(t *testing.T) {
	t.Parallel()

	lfdi, sfdi, ok := identityFromContext(context.Background())
	if ok {
		t.Fatalf("identityFromContext on bare context: ok = true, want false (lfdi=%q sfdi=%q)", lfdi, sfdi)
	}
	if lfdi != "" || sfdi != "" {
		t.Errorf("identityFromContext on bare context returned non-empty values: lfdi=%q sfdi=%q", lfdi, sfdi)
	}
}

func TestSFDIPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		sfdi    string
		want    string
		wantErr bool
	}{
		{name: "exact length", sfdi: "12345678", want: "12345678"},
		{name: "longer truncates", sfdi: "123456789012", want: "12345678"},
		{name: "too short errors", sfdi: "1234567", wantErr: true},
		{name: "empty errors", sfdi: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sfdiPrefix(tt.sfdi)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("sfdiPrefix(%q): want error, got nil (result %q)", tt.sfdi, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sfdiPrefix(%q): unexpected error: %v", tt.sfdi, err)
			}
			if got != tt.want {
				t.Errorf("sfdiPrefix(%q) = %q, want %q", tt.sfdi, got, tt.want)
			}
		})
	}
}
