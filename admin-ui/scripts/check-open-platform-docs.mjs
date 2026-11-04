import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { isDeepStrictEqual } from 'node:util'

const root = fileURLToPath(new URL('../../', import.meta.url))
const pairs = {
  'openapi.yaml': 'docs/open-platform/openapi.yaml',
  'guide.json': 'docs/open-platform/developer-guide.json',
  'signing-v1.md': 'docs/open-platform/signing-v1.md',
  'signing-vectors.json': 'docs/open-platform/signing-vectors.json',
  'OwlClient.go': 'examples/open-platform/go/main.go',
  'owl_client.py': 'examples/open-platform/python/owl_client.py',
  'OwlClient.java': 'examples/open-platform/java/OwlClient.java',
}
for (const [name, source] of Object.entries(pairs)) {
  if (!readFileSync(resolve(root, source)).equals(readFileSync(resolve(root, 'admin-ui/public/open-platform', name)))) {
    throw new Error(`在线文档与源文件不一致（${name}），请在仓库根目录运行 go run ./tools/openplatform-docs`)
  }
}
// 主契约使用合法的 JSON 格式 YAML，构建阶段无需引入额外 YAML 依赖。
if (!isDeepStrictEqual(JSON.parse(readFileSync(resolve(root, pairs['openapi.yaml']), 'utf8')), JSON.parse(readFileSync(resolve(root, 'admin-ui/public/open-platform/openapi.json'), 'utf8')))) {
  throw new Error('在线 OpenAPI JSON 已过期，请运行 go run ./tools/openplatform-docs')
}
console.log('在线文档与契约、指南和三种语言示例一致')
