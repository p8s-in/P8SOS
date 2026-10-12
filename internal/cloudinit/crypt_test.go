package cloudinit

import (
	"strings"
	"testing"
)

func TestSHA512CryptVectors(t *testing.T) {
	// From the SHA-crypt specification (Drepper).
	cases := []struct {
		pw, salt string
		rounds   int
		want     string
	}{
		{"Hello world!", "saltstring", 5000,
			"$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{"Hello world!", "saltstringsaltstring", 10000,
			"$6$rounds=10000$saltstringsaltst$OW1/O6BYHV6BcXZu8QVeXbDWra3Oeqh0sbHbbMCVNSnCM/UrjmM0Dp8vOuZeHBy/YTBmSK6H9qs/y3RnOaw5v."},
		{"This is just a test", "toolongsaltstring", 5000,
			"$6$toolongsaltstrin$lQ8jolhgVRVhY4b5pZKaysCLi0QBxGoNeKQzQ3glMhwllF7oGDZxUhx1yxdYcz/e1JSbq3y6JMxxl8audkUEm0"},
		{"we have a short salt string but not a short password", "short", 77777,
			"$6$rounds=77777$short$WuQyW2YR.hBNpjjRhpYD/ifIw05xdfeEyQoMxIXbkvr0gge1a1x3yRULJ5CCaUeOxFmtlcGZelFl5CxtgfiAc0"},
	}
	for _, c := range cases {
		if got := sha512Crypt(c.pw, c.salt, c.rounds); got != c.want {
			t.Errorf("%q/%q:\n got %s\nwant %s", c.pw, c.salt, got, c.want)
		}
	}
}

func TestHashPasswordIsSalted(t *testing.T) {
	a, b := HashPassword("secret-pass"), HashPassword("secret-pass")
	if a == b || !strings.HasPrefix(a, "$6$") || len(a) != 3+16+1+86 {
		t.Fatalf("hashes %q %q", a, b)
	}
	if strings.Contains(a, "secret") {
		t.Fatal("plaintext leaked into hash")
	}
}
