package docs

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
)

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
	for _, name := range []string{"host", "device_history", "direct_tcp_download", "upstreams", "log"} {
		if _, ok := properties[name]; !ok {
			t.Errorf("Swagger SIP config is missing %s", name)
		}
	}
	upstream, ok := definitions["api.SwaggerSIPUpstream"].(map[string]any)
	if !ok {
		t.Fatal("Swagger is missing api.SwaggerSIPUpstream")
	}
	upstreamProperties := upstream["properties"].(map[string]any)
	for _, name := range []string{"server_id", "transport", "tls_ca", "tls_cert", "tls_key", "tls_server_name", "local_id", "local_domain", "local_host", "local_port", "version", "shared_channels", "channel_id_map", "media_allowed_cidrs"} {
		if _, ok := upstreamProperties[name]; !ok {
			t.Errorf("Swagger upstream config is missing %s", name)
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
