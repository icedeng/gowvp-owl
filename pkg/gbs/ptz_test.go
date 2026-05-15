package gbs

import "testing"

func TestEncodePTZCommandZoomBits(t *testing.T) {
	tests := []struct {
		name string
		in   *PTZInput
		want string
	}{
		{
			name: "zoom in uses bit5",
			in: &PTZInput{
				Action: PTZActionZoomIn,
				Speed:  40,
			},
			want: "A50F01202828F015",
		},
		{
			name: "zoom out uses bit4",
			in: &PTZInput{
				Action: PTZActionZoomOut,
				Speed:  40,
			},
			want: "A50F01102828F005",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := encodePTZCommand(tt.in)
			if err != nil {
				t.Fatalf("encodePTZCommand() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("encodePTZCommand() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNormalizeZoomSpeed(t *testing.T) {
	tests := []struct {
		name  string
		speed uint8
		want  uint8
	}{
		{name: "stop remains zero", speed: 0, want: 0},
		{name: "protocol range unchanged", speed: 8, want: 8},
		{name: "large api speed clamps to four bit max", speed: 40, want: 15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeZoomSpeed(tt.speed); got != tt.want {
				t.Fatalf("normalizeZoomSpeed(%d) = %d, want %d", tt.speed, got, tt.want)
			}
		})
	}
}
