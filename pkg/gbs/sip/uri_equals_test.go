package sip

import "testing"

func TestURIEqualsFollowsRFC3261ComparisonRules(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		equal bool
	}{
		{
			name:  "host parameter names and standard values are case insensitive",
			left:  "sip:%61lice@atlanta.com;%74ransport=TCP",
			right: "SIP:alice@AtLaNtA.CoM;Transport=tcp",
			equal: true,
		},
		{
			name:  "extension parameter present on one side is ignored",
			left:  "sip:alice@atlanta.com;vendor=owl",
			right: "sip:alice@atlanta.com",
			equal: true,
		},
		{
			name:  "uri headers compare independent of order",
			left:  "sip:alice@atlanta.com?subject=project%20x&priority=urgent",
			right: "sip:alice@ATLANTA.com?Priority=urgent&Subject=project%20x",
			equal: true,
		},
		{
			name:  "user remains case sensitive",
			left:  "sip:Alice@atlanta.com",
			right: "sip:alice@atlanta.com",
			equal: false,
		},
		{
			name:  "explicit port does not equal omitted port",
			left:  "sip:alice@atlanta.com:5060",
			right: "sip:alice@atlanta.com",
			equal: false,
		},
		{
			name:  "method parameter cannot appear on one side only",
			left:  "sip:alice@atlanta.com;method=REGISTER",
			right: "sip:alice@atlanta.com",
			equal: false,
		},
		{
			name:  "maddr parameter cannot appear on one side only",
			left:  "sip:alice@atlanta.com;maddr=239.255.255.1",
			right: "sip:alice@atlanta.com",
			equal: false,
		},
		{
			name:  "common extension parameter values must match",
			left:  "sip:alice@atlanta.com;vendor=owl",
			right: "sip:alice@atlanta.com;vendor=other",
			equal: false,
		},
		{
			name:  "uri header values remain case sensitive",
			left:  "sip:alice@atlanta.com?subject=ProjectX",
			right: "sip:alice@atlanta.com?subject=projectx",
			equal: false,
		},
		{
			name:  "sip and sips schemes differ",
			left:  "sip:alice@atlanta.com",
			right: "sips:alice@atlanta.com",
			equal: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			left, err := ParseSipURI(test.left)
			if err != nil {
				t.Fatal(err)
			}
			right, err := ParseSipURI(test.right)
			if err != nil {
				t.Fatal(err)
			}
			if got := left.Equals(&right); got != test.equal {
				t.Fatalf("URI.Equals(%q, %q) = %t, want %t", test.left, test.right, got, test.equal)
			}
			if got := right.Equals(&left); got != test.equal {
				t.Fatalf("reverse URI.Equals(%q, %q) = %t, want %t", test.right, test.left, got, test.equal)
			}
		})
	}
}

func TestURIEqualsTreatsNilParameterSetsAsEmpty(t *testing.T) {
	left := &URI{FHost: "atlanta.com"}
	right := &URI{FHost: "ATLANTA.COM", FUriParams: NewParams(), FHeaders: NewParams()}
	if !left.Equals(right) || !right.Equals(left) {
		t.Fatal("nil and empty URI parameter sets should compare equal")
	}

	var nilURI *URI
	if !nilURI.Equals(nilURI) {
		t.Fatal("nil URI should equal itself")
	}
	if nilURI.Equals(left) {
		t.Fatal("nil URI should not equal a non-nil URI")
	}
}
