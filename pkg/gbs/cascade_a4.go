package gbs

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/gowvp/owl/internal/core/ipc"
)

func extractCascadeCatalogExtraInfo(ext *ipc.GBCatalogExt, platform cascadePlatform, localID, exposedID string) []string {
	if ext == nil {
		return nil
	}
	values := make([]string, 0, 2)
	for _, raw := range []string{ext.RawXML, wrapCascadeInfoXML(ext.InfoRawXML)} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var root a4XMLNode
		if err := xml.Unmarshal([]byte(raw), &root); err != nil {
			continue
		}
		collectCascadeExtraInfo(root, &values)
	}
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		rewritten, err := rewriteCascadeOpaqueIdentifiers(value, "ExtraInfo", platform, localID, exposedID)
		if err != nil || strings.TrimSpace(rewritten) == "" {
			continue
		}
		if _, ok := seen[rewritten]; ok {
			continue
		}
		seen[rewritten] = struct{}{}
		out = append(out, rewritten)
	}
	return out
}

func withCascadeIdentifierMapping(platform cascadePlatform, sourceID, exposedID string) cascadePlatform {
	sourceID = strings.TrimSpace(sourceID)
	exposedID = strings.TrimSpace(exposedID)
	if sourceID == "" || exposedID == "" || platform.channelIDMap[sourceID] != "" {
		return platform
	}
	clone := make(map[string]string, len(platform.channelIDMap)+1)
	for key, value := range platform.channelIDMap {
		clone[key] = value
	}
	clone[sourceID] = exposedID
	platform.channelIDMap = clone
	return platform
}

func wrapCascadeInfoXML(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	return "<Info>" + raw + "</Info>"
}

func collectCascadeExtraInfo(node a4XMLNode, out *[]string) {
	name := strings.TrimSpace(node.XMLName.Local)
	if strings.EqualFold(name, "ExtraInfo") || strings.EqualFold(name, "ExtralInfo") {
		if value := strings.TrimSpace(collectNodeText(node)); value != "" {
			*out = append(*out, value)
		}
	}
	for _, child := range node.Children {
		collectCascadeExtraInfo(child, out)
	}
}

func rewriteCascadeIdentifierValue(value, field string, platform cascadePlatform, localID, exposedID string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if strings.EqualFold(strings.TrimSpace(field), "ParentID") {
		return platform.localID, nil
	}
	if !isGBDeviceIdentifier(trimmed) {
		return value, nil
	}
	if trimmed == strings.TrimSpace(localID) {
		return strings.TrimSpace(exposedID), nil
	}
	if mapped := platform.channelIDMap[trimmed]; mapped != "" {
		return mapped, nil
	}
	if trimmed == platform.localID || platform.exposedChannelMap[trimmed] != "" {
		return trimmed, nil
	}
	return "", fmt.Errorf("unshared cascade identifier in %s", strings.TrimSpace(field))
}

func rewriteCascadeOpaqueIdentifiers(value, field string, platform cascadePlatform, localID, exposedID string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value, nil
	}
	var document any
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		if decoded, ok := decodeExtraInfoJSON(trimmed); ok {
			document = decoded
		}
	}
	if document != nil {
		rewritten, err := rewriteCascadeJSONIdentifiers(document, field, platform, localID, exposedID)
		if err != nil {
			return "", err
		}
		body, err := json.Marshal(rewritten)
		if err != nil {
			return "", err
		}
		return string(body), nil
	}

	rewritten := value
	known := make(map[string]string, len(platform.channelIDMap)+1)
	if isGBDeviceIdentifier(strings.TrimSpace(localID)) && strings.TrimSpace(exposedID) != "" {
		known[strings.TrimSpace(localID)] = strings.TrimSpace(exposedID)
	}
	for sourceID, mappedID := range platform.channelIDMap {
		known[sourceID] = mappedID
	}
	for sourceID, mappedID := range known {
		rewritten = strings.ReplaceAll(rewritten, sourceID, mappedID)
	}
	for _, identifier := range findGBDeviceIdentifiers(rewritten) {
		if identifier == platform.localID || platform.exposedChannelMap[identifier] != "" {
			continue
		}
		return "", fmt.Errorf("unshared cascade identifier in %s", strings.TrimSpace(field))
	}
	return rewritten, nil
}

func rewriteCascadeJSONIdentifiers(value any, field string, platform cascadePlatform, localID, exposedID string) (any, error) {
	switch item := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, child := range item {
			rewritten, err := rewriteCascadeJSONIdentifiers(child, key, platform, localID, exposedID)
			if err != nil {
				return nil, err
			}
			out[key] = rewritten
		}
		return out, nil
	case []any:
		out := make([]any, len(item))
		for index, child := range item {
			rewritten, err := rewriteCascadeJSONIdentifiers(child, field, platform, localID, exposedID)
			if err != nil {
				return nil, err
			}
			out[index] = rewritten
		}
		return out, nil
	case string:
		if isGBDeviceIdentifier(strings.TrimSpace(item)) {
			return rewriteCascadeIdentifierValue(item, field, platform, localID, exposedID)
		}
		return rewriteCascadeOpaqueIdentifiers(item, field, platform, localID, exposedID)
	case json.Number:
		raw := strings.TrimSpace(item.String())
		if !isGBDeviceIdentifier(raw) {
			return item, nil
		}
		rewritten, err := rewriteCascadeIdentifierValue(raw, field, platform, localID, exposedID)
		if err != nil {
			return nil, err
		}
		return json.Number(rewritten), nil
	default:
		return value, nil
	}
}

func isGBDeviceIdentifier(value string) bool {
	if len(value) != 20 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func findGBDeviceIdentifiers(value string) []string {
	out := make([]string, 0, 2)
	for start := 0; start < len(value); {
		if value[start] < '0' || value[start] > '9' {
			start++
			continue
		}
		end := start + 1
		for end < len(value) && value[end] >= '0' && value[end] <= '9' {
			end++
		}
		if end-start == 20 {
			out = append(out, value[start:end])
		}
		start = end
	}
	return out
}
