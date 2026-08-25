package conf

import (
	"strings"
	"testing"
)

func validMonitorUserIdentityConfig() SIPMonitorUserIdentity {
	return SIPMonitorUserIdentity{
		Enabled:              true,
		LocalGatewayID:       "34020000002110000001",
		RemoteGatewayID:      "34020000002110000002",
		LocalUserID:          "34020000003000000001",
		LocalOrganization:    "340200",
		LocalCategory:        "operator",
		LocalRank:            "level1",
		TrustedGatewayIDs:    []string{"34030000002110000003"},
		AllowedUserIDs:       []string{"34030000003000000001"},
		AllowedOrganizations: []string{"340300"},
		AllowedCategories:    []string{"operator"},
		AllowedRanks:         []string{"level2"},
		MaxHops:              8,
	}
}

func TestValidateMonitorUserIdentityConfig(t *testing.T) {
	if err := ValidateMonitorUserIdentityConfig(SIPMonitorUserIdentity{}); err != nil {
		t.Fatalf("disabled Monitor-User-Identity config should remain compatible: %v", err)
	}
	valid := validMonitorUserIdentityConfig()
	if err := ValidateMonitorUserIdentityConfig(valid); err != nil {
		t.Fatalf("valid Monitor-User-Identity config rejected: %v", err)
	}
	valid.Enabled = false
	valid.Required = true
	if err := ValidateMonitorUserIdentityConfig(valid); err != nil {
		t.Fatalf("Required should implicitly enable Monitor-User-Identity: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*SIPMonitorUserIdentity)
		want   string
	}{
		{name: "local gateway type", mutate: func(config *SIPMonitorUserIdentity) { config.LocalGatewayID = "34020000002000000001" }, want: "本地安全路由网关"},
		{name: "remote gateway type", mutate: func(config *SIPMonitorUserIdentity) { config.RemoteGatewayID = "34020000002000000002" }, want: "上级安全路由网关"},
		{name: "same gateway", mutate: func(config *SIPMonitorUserIdentity) { config.RemoteGatewayID = config.LocalGatewayID }, want: "不能相同"},
		{name: "local user type", mutate: func(config *SIPMonitorUserIdentity) { config.LocalUserID = "34020000002110000003" }, want: "本域用户 ID"},
		{name: "attribute delimiter", mutate: func(config *SIPMonitorUserIdentity) { config.LocalOrganization = "3402-00" }, want: "分段符号"},
		{name: "attribute control", mutate: func(config *SIPMonitorUserIdentity) { config.LocalCategory = "operator\r\ninjected" }, want: "控制字符"},
		{name: "max hops", mutate: func(config *SIPMonitorUserIdentity) { config.MaxHops = 33 }, want: "max_hops"},
		{name: "minimum hops", mutate: func(config *SIPMonitorUserIdentity) { config.MaxHops = 1 }, want: "max_hops"},
		{name: "required authorization", mutate: func(config *SIPMonitorUserIdentity) {
			config.Required = true
			config.AllowedUserIDs = nil
			config.AllowedOrganizations = nil
			config.AllowedCategories = nil
			config.AllowedRanks = nil
		}, want: "至少一项"},
		{name: "trusted local gateway", mutate: func(config *SIPMonitorUserIdentity) { config.TrustedGatewayIDs = []string{config.LocalGatewayID} }, want: "不能包含本地网关"},
		{name: "trusted duplicate", mutate: func(config *SIPMonitorUserIdentity) { config.TrustedGatewayIDs = []string{config.RemoteGatewayID} }, want: "可信网关重复"},
		{name: "allowed user", mutate: func(config *SIPMonitorUserIdentity) { config.AllowedUserIDs = []string{"not-a-user"} }, want: "允许用户"},
		{name: "allowed attribute", mutate: func(config *SIPMonitorUserIdentity) { config.AllowedRanks = []string{"level-2"} }, want: "允许职级"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validMonitorUserIdentityConfig()
			test.mutate(&config)
			err := ValidateMonitorUserIdentityConfig(config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateMonitorUserIdentityConfig() error = %v, want substring %q", err, test.want)
			}
		})
	}
}
