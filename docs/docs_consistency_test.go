package docs

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestGB28181ZLMediaKitDeploymentUsesPatchedCommit(t *testing.T) {
	const zlmCommit = "2fa539807370c4bcfca9801bde1dd2fdd95ba113"
	const patchPath = "third_party/zlmediakit/patches/0001-gb28181-tcp-rtcp-rfc4571.patch"
	for _, name := range []string{"../Dockerfile_zlm", "../Dockerfile_ai"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if !strings.Contains(text, "ARG ZLM_COMMIT="+zlmCommit) {
			t.Errorf("%s does not pin the audited ZLMediaKit commit", name)
		}
		if !strings.Contains(text, "COPY "+patchPath) || !strings.Contains(text, "git apply --check") {
			t.Errorf("%s does not apply the GB28181 TCP RTCP patch", name)
		}
		if strings.Contains(text, "zlmediakit/zlmediakit:master") {
			t.Errorf("%s still uses the floating ZLMediaKit master tag", name)
		}
	}

	compose, err := os.ReadFile("../docker-compose.debug.yml")
	if err != nil {
		t.Fatal(err)
	}
	composeText := string(compose)
	if !strings.Contains(composeText, "target: zlm-runtime") || !strings.Contains(composeText, "ZLM_COMMIT: "+zlmCommit) {
		t.Error("docker-compose.debug.yml does not build the patched ZLMediaKit runtime")
	}
	if strings.Count(composeText, "OWL_MEDIA_SECRET: ${OWL_MEDIA_SECRET:?") != 2 {
		t.Error("docker-compose.debug.yml must inject the same required media secret into Owl and ZLMediaKit")
	}
	for _, marker := range []string{"OWL_MEDIA_IP: zlm", "OWL_MEDIA_HTTP_PORT: \"80\"", "OWL_MEDIA_WEBHOOK_IP: gowvp", "zlm-config:/opt/media/conf"} {
		if !strings.Contains(composeText, marker) {
			t.Errorf("docker-compose.debug.yml is missing %s", marker)
		}
	}
	if strings.Contains(composeText, "./configs:/opt/media/conf") {
		t.Error("docker-compose.debug.yml still hides the ZLMediaKit config template with the Owl config directory")
	}

	patchBody, err := os.ReadFile("../" + patchPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"tcp_rtcp", "sendRtcpOverTcp", "onSendRtpTcp"} {
		if !strings.Contains(string(patchBody), marker) {
			t.Errorf("ZLMediaKit patch is missing %s", marker)
		}
	}
}

func TestZLMediaKitEntrypointSynchronizesSecret(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.ini")
	configPath := filepath.Join(dir, "runtime", "config.ini")
	if err := os.WriteFile(templatePath, []byte("[api]\nsecret=upstream-default\n[http]\nport=80\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(secret string) error {
		cmd := exec.Command("sh", "../scripts/zlm-entrypoint.sh", "true")
		cmd.Env = append(os.Environ(),
			"ZLM_CONFIG_PATH="+configPath,
			"ZLM_CONFIG_TEMPLATE="+templatePath,
			"OWL_MEDIA_SECRET="+secret,
			"ZLM_API_DEBUG=0",
		)
		return cmd.Run()
	}
	for _, secret := range []string{"first-shared-secret", "rotated-shared-secret"} {
		if err := run(secret); err != nil {
			t.Fatalf("entrypoint failed: %v", err)
		}
		body, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "[api]\nsecret="+secret+"\n") {
			t.Fatalf("runtime ZLMediaKit secret was not synchronized: %s", body)
		}
		if strings.Count(string(body), "secret=") != 1 {
			t.Fatalf("runtime ZLMediaKit config contains duplicate secrets: %s", body)
		}
		if strings.Count(string(body), "apiDebug=0") != 1 {
			t.Fatalf("runtime ZLMediaKit config must disable secret-bearing API debug logs by default: %s", body)
		}
	}
	if err := os.WriteFile(configPath, []byte("api.secret=old\n[api]\nsecret=duplicate\nsecret=duplicate-again\napiDebug=1\napiDebug=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run("deduplicated-secret"); err != nil {
		t.Fatalf("entrypoint failed to deduplicate secrets: %v", err)
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "secret=") != 1 || !strings.Contains(string(body), "secret=deduplicated-secret") || strings.Count(string(body), "apiDebug=0") != 1 {
		t.Fatalf("runtime ZLMediaKit config did not collapse duplicate secrets: %s", body)
	}
	if strings.Count(string(body), "[api]") != 1 {
		t.Fatalf("runtime ZLMediaKit config contains duplicate api sections: %s", body)
	}
	if err := os.WriteFile(configPath, []byte("[api]\napiDebug=1\n[http]\nport=80\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run("inserted-secret"); err != nil {
		t.Fatalf("entrypoint failed to complete an existing api section: %v", err)
	}
	body, err = os.ReadFile(configPath)
	if err != nil || strings.Count(string(body), "[api]") != 1 || !strings.Contains(string(body), "[api]\nsecret=inserted-secret\napiDebug=0\n") {
		t.Fatalf("runtime ZLMediaKit config did not complete the existing api section: %s, %v", body, err)
	}
	if err := os.WriteFile(configPath, []byte("api.secret=old\napi.apiDebug=1\napi.apiDebug=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run("flat-secret"); err != nil {
		t.Fatalf("entrypoint failed to rewrite a flat api config: %v", err)
	}
	body, err = os.ReadFile(configPath)
	if err != nil || strings.Contains(string(body), "[api]") || strings.Count(string(body), "api.secret=flat-secret") != 1 || strings.Count(string(body), "api.apiDebug=0") != 1 {
		t.Fatalf("runtime ZLMediaKit flat api config was not normalized: %s, %v", body, err)
	}
	if err := os.WriteFile(configPath, []byte("[api]\nsecret=keep-me\napiDebug=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(""); err != nil {
		t.Fatalf("entrypoint failed without an injected media secret: %v", err)
	}
	body, err = os.ReadFile(configPath)
	if err != nil || !strings.Contains(string(body), "secret=keep-me") || strings.Count(string(body), "apiDebug=0") != 1 {
		t.Fatalf("runtime ZLMediaKit config did not preserve the existing secret while disabling api debug: %s, %v", body, err)
	}
	cmd := exec.Command("sh", "../scripts/zlm-entrypoint.sh", "true")
	cmd.Env = append(os.Environ(),
		"ZLM_CONFIG_PATH="+configPath,
		"ZLM_CONFIG_TEMPLATE="+templatePath,
		"OWL_MEDIA_SECRET=debug-secret",
		"ZLM_API_DEBUG=1",
	)
	if err := cmd.Run(); err != nil {
		t.Fatalf("entrypoint rejected explicit API debug mode: %v", err)
	}
	body, err = os.ReadFile(configPath)
	if err != nil || strings.Count(string(body), "apiDebug=1") != 1 {
		t.Fatalf("runtime ZLMediaKit config did not enable explicit API debug mode: %s, %v", body, err)
	}
	if err := run("invalid\nsecret"); err == nil {
		t.Fatal("entrypoint accepted a media secret containing a newline")
	}
	cmd = exec.Command("sh", "../scripts/zlm-entrypoint.sh", "true")
	cmd.Env = append(os.Environ(),
		"ZLM_CONFIG_PATH="+configPath,
		"ZLM_CONFIG_TEMPLATE="+templatePath,
		"OWL_MEDIA_SECRET=valid-secret",
		"ZLM_API_DEBUG=2",
	)
	if err := cmd.Run(); err == nil {
		t.Fatal("entrypoint accepted an invalid API debug value")
	}
}

func TestSwaggerGoTemplateMatchesJSON(t *testing.T) {
	fileBody, err := os.ReadFile("swagger.json")
	if err != nil {
		t.Fatal(err)
	}
	var fileDoc any
	if err := json.Unmarshal(fileBody, &fileDoc); err != nil {
		t.Fatalf("decode swagger.json: %v", err)
	}

	var generatedDoc any
	if err := json.Unmarshal([]byte(SwaggerInfo.ReadDoc()), &generatedDoc); err != nil {
		if syntax, ok := err.(*json.SyntaxError); ok {
			body := SwaggerInfo.ReadDoc()
			start := max(0, int(syntax.Offset)-80)
			end := min(len(body), int(syntax.Offset)+80)
			t.Fatalf("decode docs.go Swagger template at byte %d: %v; context=%q", syntax.Offset, err, body[start:end])
		}
		t.Fatalf("decode docs.go Swagger template: %v", err)
	}
	fileObject := fileDoc.(map[string]any)
	generatedObject := generatedDoc.(map[string]any)
	for _, section := range []string{"paths", "definitions"} {
		if !reflect.DeepEqual(generatedObject[section], fileObject[section]) {
			t.Fatalf("docs.go Swagger template and swagger.json are inconsistent: %s", firstSwaggerDifference(generatedObject[section], fileObject[section], "$."+section))
		}
	}
}

func TestSwaggerSIPConfigDocumentsCascadeFields(t *testing.T) {
	body, err := os.ReadFile("swagger.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	if _, ok := paths["/configs/info/sip"]; !ok {
		t.Fatal("Swagger is missing PUT /configs/info/sip")
	}
	definitions := document["definitions"].(map[string]any)
	config, ok := definitions["api.SwaggerSIPConfig"].(map[string]any)
	if !ok {
		t.Fatal("Swagger is missing api.SwaggerSIPConfig")
	}
	properties := config["properties"].(map[string]any)
	for _, name := range []string{"host", "tls_client_ca", "tls_require_client_cert", "register_redirect", "register_certificate_auth", "signal_digest", "device_history", "direct_tcp_download", "annex_g", "upstreams", "log"} {
		if _, ok := properties[name]; !ok {
			t.Errorf("Swagger SIP config is missing %s", name)
		}
	}
	annexGSystem, ok := definitions["api.SwaggerSIPAnnexGSystem"].(map[string]any)
	if !ok {
		t.Fatal("Swagger is missing api.SwaggerSIPAnnexGSystem")
	}
	if _, ok := annexGSystem["properties"].(map[string]any)["signal_digest_seed"]; !ok {
		t.Error("Swagger Annex G system config is missing signal_digest_seed")
	}
	upstream, ok := definitions["api.SwaggerSIPUpstream"].(map[string]any)
	if !ok {
		t.Fatal("Swagger is missing api.SwaggerSIPUpstream")
	}
	upstreamProperties := upstream["properties"].(map[string]any)
	for _, name := range []string{"server_id", "transport", "tls_ca", "tls_cert", "tls_key", "tls_server_name", "local_id", "local_domain", "local_host", "local_port", "version", "alarm_dispatch_enabled", "shared_channels", "channel_id_map", "media_allowed_cidrs"} {
		if _, ok := upstreamProperties[name]; !ok {
			t.Errorf("Swagger upstream config is missing %s", name)
		}
	}
}

func TestSwaggerGBDeviceConfigDocumentsExtraInfo(t *testing.T) {
	body, err := os.ReadFile("swagger.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	definitions := document["definitions"].(map[string]any)
	config, ok := definitions["api.SwaggerGBDeviceConfigInput"].(map[string]any)
	if !ok {
		t.Fatal("Swagger is missing api.SwaggerGBDeviceConfigInput")
	}
	extraInfo, ok := config["properties"].(map[string]any)["extra_info"].(map[string]any)
	if !ok || extraInfo["type"] != "array" {
		t.Fatal("Swagger DeviceConfig is missing the 2022 extra_info array")
	}
}

func TestSwaggerGBSubscriptionStatesDocumentsRuntimeFields(t *testing.T) {
	body, err := os.ReadFile("swagger.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}

	paths := document["paths"].(map[string]any)
	pathItem, ok := paths["/devices/{id}/subscriptions"].(map[string]any)
	if !ok {
		t.Fatal("Swagger is missing /devices/{id}/subscriptions")
	}
	operation, ok := pathItem["get"].(map[string]any)
	if !ok {
		t.Fatal("Swagger is missing GET /devices/{id}/subscriptions")
	}
	response := operation["responses"].(map[string]any)["200"].(map[string]any)
	schema := response["schema"].(map[string]any)
	if got := schema["$ref"]; got != "#/definitions/api.subscriptionStatesOutput" {
		t.Fatalf("subscription states response = %v", got)
	}

	definitions := document["definitions"].(map[string]any)
	state, ok := definitions["ipc.SubscriptionState"].(map[string]any)
	if !ok {
		t.Fatal("Swagger is missing ipc.SubscriptionState")
	}
	properties := state["properties"].(map[string]any)
	for _, name := range []string{"event", "status", "expires", "expires_at", "notify_cseq"} {
		if _, ok := properties[name]; !ok {
			t.Errorf("Swagger subscription state is missing %s", name)
		}
	}
}

func TestSwaggerGBChannelOperationResponseSchemas(t *testing.T) {
	body, err := os.ReadFile("swagger.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	assertResponseRef := func(path, method, want string) {
		t.Helper()
		pathItem, ok := paths[path].(map[string]any)
		if !ok {
			t.Fatalf("Swagger is missing %s", path)
		}
		operation, ok := pathItem[method].(map[string]any)
		if !ok {
			t.Fatalf("Swagger is missing %s %s", strings.ToUpper(method), path)
		}
		responses := operation["responses"].(map[string]any)
		response := responses["200"].(map[string]any)
		schema := response["schema"].(map[string]any)
		if got := schema["$ref"]; got != want {
			t.Fatalf("Swagger %s %s response = %v, want %s", strings.ToUpper(method), path, got, want)
		}
	}

	for _, path := range []string{
		"/channels/{id}/upgrade",
		"/channels/gb28181/{device_id}/{channel_id}/upgrade",
	} {
		assertResponseRef(path, "post", "#/definitions/ipc.UpgradeOutput")
	}
	assertResponseRef("/channels/gb28181/{device_id}/{channel_id}/ptz", "post", "#/definitions/api.SwaggerMessageResponse")
	assertResponseRef("/channels/gb28181/{device_id}/{channel_id}/upgrade/{session_id}", "get", "#/definitions/gbs.UpgradeState")
	assertResponseRef("/channels/gb28181/{device_id}/{channel_id}/snapshot/{session_id}/status", "get", "#/definitions/gbs.SnapshotState")
}

func TestSwaggerGBDeviceQueryDocumentsAllPublicActions(t *testing.T) {
	body, err := os.ReadFile("swagger.json")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	pathItem := paths["/devices/{id}/gb/query"].(map[string]any)
	operation := pathItem["post"].(map[string]any)
	description := operation["description"].(string)
	for _, action := range []string{
		"device_status", "catalog", "config_download", "preset_query", "record_info", "mobile_position",
		"home_position_query", "cruise_track_list", "cruise_track", "ptz_position", "sdcard_status",
	} {
		if !strings.Contains(description, "`"+action+"`") {
			t.Errorf("Swagger GB query description is missing %s", action)
		}
	}
}

func firstSwaggerDifference(left, right any, path string) string {
	if reflect.DeepEqual(left, right) {
		return ""
	}
	switch leftValue := left.(type) {
	case map[string]any:
		rightValue, ok := right.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s type differs", path)
		}
		keys := make([]string, 0, len(leftValue)+len(rightValue))
		seen := make(map[string]struct{}, len(leftValue)+len(rightValue))
		for key := range leftValue {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
		for key := range rightValue {
			if _, ok := seen[key]; !ok {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			leftItem, leftOK := leftValue[key]
			rightItem, rightOK := rightValue[key]
			if !leftOK || !rightOK {
				return fmt.Sprintf("%s.%s presence differs", path, key)
			}
			if diff := firstSwaggerDifference(leftItem, rightItem, path+"."+key); diff != "" {
				return diff
			}
		}
	case []any:
		rightValue, ok := right.([]any)
		if !ok || len(leftValue) != len(rightValue) {
			return fmt.Sprintf("%s array differs", path)
		}
		for index := range leftValue {
			if diff := firstSwaggerDifference(leftValue[index], rightValue[index], fmt.Sprintf("%s[%d]", path, index)); diff != "" {
				return diff
			}
		}
	default:
		return fmt.Sprintf("%s: %v != %v", path, left, right)
	}
	return path
}
