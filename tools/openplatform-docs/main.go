// openplatform-docs 将单一接口契约、开发指南及示例发布为可离线阅读的静态资源。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
	"github.com/gowvp/owl/internal/web/openapi"
)

func main() {
	check := flag.Bool("check", false, "只检查文档生成结果是否与源码一致")
	flag.Parse()
	raw, err := os.ReadFile("docs/open-platform/openapi.yaml")
	must(err)
	var spec map[string]any
	must(yaml.Unmarshal(raw, &spec))
	count := 0
	var validateRefs func(any)
	validateRefs = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				var target any = spec
				for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					m, ok := target.(map[string]any)
					if !ok || m[part] == nil {
						panic("无法解析文档引用: " + ref)
					}
					target = m[part]
				}
			}
			for _, child := range v {
				validateRefs(child)
			}
		case []any:
			for _, child := range v {
				validateRefs(child)
			}
		}
	}
	validateRefs(spec)
	// 文档必须覆盖全部实际路由，避免新增入口后遗漏可读契约。
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	openapi.New(nil).Register(router, func(c *gin.Context) { c.Next() })
	paths := spec["paths"].(map[string]any)
	parameter := regexp.MustCompile(`[:*]([A-Za-z_]+)`)
	actual := map[string]bool{}
	for _, route := range router.Routes() {
		path := parameter.ReplaceAllString(route.Path, "{$1}")
		methods, ok := paths[path].(map[string]any)
		if !ok || methods[strings.ToLower(route.Method)] == nil {
			panic("实际接口未写入文档: " + route.Method + " " + path)
		}
		actual[route.Method+" "+path] = true
	}
	for path, value := range spec["paths"].(map[string]any) {
		for method, op := range value.(map[string]any) {
			if !actual[strings.ToUpper(method)+" "+path] {
				panic("文档接口未在服务注册: " + method + " " + path)
			}
			row := op.(map[string]any)
			if row["summary"] == nil || row["description"] == nil || row["x-scope"] == nil {
				panic("缺少接口说明: " + method + " " + path)
			}
			count++
		}
	}
	outputs := map[string][]byte{}
	data, err := json.MarshalIndent(spec, "", "  ")
	must(err)
	outputs["openapi.json"] = append(data, '\n')
	sources := map[string]string{
		"openapi.yaml":         "docs/open-platform/openapi.yaml",
		"guide.json":           "docs/open-platform/developer-guide.json",
		"signing-v1.md":        "docs/open-platform/signing-v1.md",
		"signing-vectors.json": "docs/open-platform/signing-vectors.json",
		"OwlClient.go":         "examples/open-platform/go/main.go",
		"owl_client.py":        "examples/open-platform/python/owl_client.py",
		"OwlClient.java":       "examples/open-platform/java/OwlClient.java",
	}
	for name, source := range sources {
		b, e := os.ReadFile(source)
		must(e)
		outputs[name] = b
	}
	var guide struct {
		Sections []json.RawMessage `json:"sections"`
	}
	must(json.Unmarshal(outputs["guide.json"], &guide))
	if len(guide.Sections) == 0 {
		panic("开发指南不能为空")
	}
	outputDir := "admin-ui/public/open-platform"
	if !*check {
		must(os.MkdirAll(outputDir, 0755))
	}
	for name, b := range outputs {
		path := filepath.Join(outputDir, name)
		if *check {
			current, e := os.ReadFile(path)
			if e != nil || !bytes.Equal(current, b) {
				panic("文档已变化，请运行 go run ./tools/openplatform-docs: " + name)
			}
		} else {
			must(os.WriteFile(path, b, 0644))
		}
	}
	fmt.Printf("开发文档已%s：%d 个接口操作，%d 个指南章节，%d 个静态文件\n", map[bool]string{true: "核对", false: "生成"}[*check], count, len(guide.Sections), len(outputs))
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
