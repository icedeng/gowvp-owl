package conf

import (
	"strings"
	"testing"
)

func TestValidateRegisterCertificateAuthConfig(t *testing.T) {
	valid := SIPRegisterCertificateAuth{
		Required:     true,
		PlatformCert: "platform.crt",
		PlatformKey:  "platform.key",
		DeviceCertificates: map[string]string{
			"34020000001320000001": "device.crt",
		},
	}
	if err := ValidateRegisterCertificateAuthConfig(valid); err != nil {
		t.Fatalf("valid certificate REGISTER config rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*SIPRegisterCertificateAuth)
		want   string
	}{
		{name: "platform certificate", mutate: func(config *SIPRegisterCertificateAuth) { config.PlatformCert = "" }, want: "平台证书"},
		{name: "platform key", mutate: func(config *SIPRegisterCertificateAuth) { config.PlatformKey = "" }, want: "平台私钥"},
		{name: "device certificate", mutate: func(config *SIPRegisterCertificateAuth) { config.DeviceCertificates = nil }, want: "设备证书"},
		{name: "device id", mutate: func(config *SIPRegisterCertificateAuth) {
			config.DeviceCertificates = map[string]string{"invalid": "device.crt"}
		}, want: "20 位数字"},
		{name: "crl without ca", mutate: func(config *SIPRegisterCertificateAuth) { config.CRL = "device.crl" }, want: "设备 CA"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			config.DeviceCertificates = cloneCertificateMap(valid.DeviceCertificates)
			test.mutate(&config)
			err := ValidateRegisterCertificateAuthConfig(config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation result = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDisabledRegisterCertificateAuthRemainsBackwardCompatible(t *testing.T) {
	if err := ValidateRegisterCertificateAuthConfig(SIPRegisterCertificateAuth{}); err != nil {
		t.Fatalf("disabled certificate REGISTER config rejected: %v", err)
	}
}

func TestValidateUpstreamRegisterCertificateAuthConfig(t *testing.T) {
	valid := SIPUpstreamRegisterCertificateAuth{
		Required:   true,
		LocalCert:  "local.crt",
		LocalKey:   "local.key",
		ServerCert: "server.crt",
	}
	if err := ValidateUpstreamRegisterCertificateAuthConfig(valid); err != nil {
		t.Fatalf("valid upstream certificate REGISTER config rejected: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*SIPUpstreamRegisterCertificateAuth)
		want   string
	}{
		{name: "local cert", mutate: func(config *SIPUpstreamRegisterCertificateAuth) { config.LocalCert = "" }, want: "本平台证书"},
		{name: "local key", mutate: func(config *SIPUpstreamRegisterCertificateAuth) { config.LocalKey = "" }, want: "本平台私钥"},
		{name: "server cert", mutate: func(config *SIPUpstreamRegisterCertificateAuth) { config.ServerCert = "" }, want: "上级平台证书"},
		{name: "crl without ca", mutate: func(config *SIPUpstreamRegisterCertificateAuth) { config.CRL = "server.crl" }, want: "上级平台 CA"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			err := ValidateUpstreamRegisterCertificateAuthConfig(config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation result = %v, want %q", err, test.want)
			}
		})
	}
}

func cloneCertificateMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
