package wallet

import "testing"

func TestParseAddressRefusesSmallOrderKeys(t *testing.T) {
	for _, s := range []string{
		"11111111111111111111111111111111",
		"11111111111111111111111111111113D",
		"4uQeVj5tqViQh7yWWGStvkEG1Zmhx6uasJtWCJziofM",
		"4uQeVj5tqViQh7yWWGStvkEG1Zmhx6uasJtWCJziohZ",
		"3ctC68zTqpRDQShoondiQKDHwZDAUjRyxiPNdg8cD6Pe",
		"3ctC68zTqpRDQShoondiQKDHwZDAUjRyxiPNdg8cD6Rr",
		"EQAqmjhcsBQhpBv5GJkYgEB7emGHZNoo1j1yAjiFLNvD",
		"EQAqmjhcsBQhpBv5GJkYgEB7emGHZNoo1j1yAjiFLNxR",
		"Gx9dDNxzpALCowVuZb7pBceBLJugLA8sPa6TJDXrpfeW",
		"Gx9dDNxzpALCowVuZb7pBceBLJugLA8sPa6TJDXrpfgi",
		"H242rsh5hzpvDdct56PG5YPQbKUT37EmySQLoQqrYUJr",
		"H242rsh5hzpvDdct56PG5YPQbKUT37EmySQLoQqrYUM4",
		"H5xSWNRAbqKddKjrabehyU8drL3Dk4LgZJiEJc9rGGyC",
		"H5xSWNRAbqKddKjrabehyU8drL3Dk4LgZJiEJc9rGH1Q",
	} {
		if a, err := ParseAddress(s); err == nil {
			t.Errorf("ParseAddress(%q) = %s, want a small-order refusal", s, a)
		}
	}
	const real = "FeScpzDG56MmjDy2jf2FdbwPF9BK8hvHed4gM7wnCjof"
	a, err := ParseAddress(real)
	if err != nil {
		t.Fatalf("ParseAddress(%q): %v", real, err)
	}
	if a.String() != real {
		t.Errorf("ParseAddress(%q).String() = %s", real, a)
	}
}
