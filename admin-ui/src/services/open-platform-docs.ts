export interface Schema {
  $ref?: string; type?: string; format?: string; description?: string; nullable?: boolean;
  properties?: Record<string, Schema>; required?: string[]; items?: Schema; allOf?: Schema[];
  enum?: unknown[]; default?: unknown; example?: unknown; minimum?: number; maximum?: number;
  maxLength?: number; minLength?: number; maxItems?: number; additionalProperties?: boolean;
}
export interface Parameter { $ref?: string; name: string; in: string; required?: boolean; description?: string; schema: Schema }
export interface Response { $ref?: string; description?: string; content?: Record<string, {schema: Schema;example?:unknown}> }
export interface Operation {
  operationId: string; summary: string; description: string; tags: string[]; 'x-scope': string;
  parameters?: Parameter[]; requestBody?: {description?: string; required?: boolean; content: Record<string,{schema: Schema;example?:unknown}>};
  responses: Record<string, Response>;
}
export interface Spec {
  info: {version: string; title: string}; paths: Record<string,Record<string,Operation>>;
  components: {schemas: Record<string,Schema>; parameters: Record<string,Parameter>; responses: Record<string,Response>};
}
export interface Endpoint extends Operation { path: string; method: string }
export interface GuideBlock { title: string; paragraphs?: string[]; steps?: string[]; columns?: string[]; rows?: string[][]; code?: string; language?: string }
export interface GuideSection { id: string; title: string; intro: string; blocks: GuideBlock[] }
export interface PublicConfig { enabled: boolean; public_url: string; audience: string; allow_http: boolean }
export interface FieldRow { name: string; type: string; required: boolean; description: string; constraints: string }

export function documentAsset(name: string) { return new URL(`open-platform/${name}`, document.baseURI).href }
export function dereference<T extends {$ref?: string}>(spec: Spec, value: T): T {
  if (!value.$ref) return value
  let result: unknown = spec
  for (const part of value.$ref.replace(/^#\//,'').split('/')) result=(result as Record<string,unknown>)[part]
  return {...result as T, ...Object.fromEntries(Object.entries(value).filter(([key])=>key!=='$ref'))} as T
}
export function resolveSchema(spec: Spec, value: Schema): Schema {
  const schema=dereference(spec,value)
  if (!schema.allOf) return schema
  const merged: Schema={...schema,properties:{...schema.properties},required:[...(schema.required||[])]}
  for (const piece of schema.allOf) {
    const part=resolveSchema(spec,piece)
    Object.assign(merged.properties!,part.properties)
    merged.required!.push(...(part.required||[]))
    merged.type ||= part.type
  }
  return merged
}
export function schemaType(schema: Schema): string {
  if (schema.type==='array') return `${schema.items?.$ref?.split('/').pop() || schema.items?.type || 'object'}[]`
  return schema.type || (schema.$ref?.split('/').pop()) || 'object'
}
export function schemaConstraints(schema: Schema): string {
  return [schema.enum?`可选值：${schema.enum.join(' / ')}`:'',schema.default!==undefined?`默认：${JSON.stringify(schema.default)}`:'',schema.minimum!==undefined?`最小：${schema.minimum}`:'',schema.maximum!==undefined?`最大：${schema.maximum}`:'',schema.maxLength!==undefined?`最长：${schema.maxLength} 字符`:'',schema.maxItems!==undefined?`最多：${schema.maxItems} 项`:'',schema.nullable?'可为 null':''].filter(Boolean).join('；')
}
export function schemaFields(spec: Spec, value: Schema, prefix='', depth=0): FieldRow[] {
  if(depth>7) return []
  const schema=resolveSchema(spec,value), rows: FieldRow[]=[]
  for(const [name,raw] of Object.entries(schema.properties||{})) {
    const child=resolveSchema(spec,raw), path=prefix+name
    rows.push({name:path,type:schemaType(child),required:(schema.required||[]).includes(name),description:child.description||raw.description||'见嵌套字段说明。',constraints:schemaConstraints(child)})
    if(child.properties) rows.push(...schemaFields(spec,child,`${path}.`,depth+1))
    else if(child.type==='array' && child.items) rows.push(...schemaFields(spec,child.items,`${path}[].`,depth+1))
  }
  return rows
}
export function schemaExample(spec: Spec,value: Schema,depth=0): unknown {
  const schema=resolveSchema(spec,value)
  if (schema.example!==undefined) return schema.example
  if (schema.default!==undefined) return schema.default
  if (depth>8) return null
  if (schema.properties) return Object.fromEntries(Object.entries(schema.properties).map(([key,v])=>[key,schemaExample(spec,v,depth+1)]))
  if (schema.type==='array') return schema.items?[schemaExample(spec,schema.items,depth+1)]:[]
  if (schema.enum?.length) return schema.enum[0]
  if (schema.nullable) return null
  if (schema.type==='boolean') return true
  if (schema.type==='integer'||schema.type==='number') return schema.minimum??1
  return schema.format==='date-time'?'2026-11-04T08:00:00Z':'示例值'
}
export function requestExample(spec: Spec, endpoint: Endpoint): unknown {
  const content=endpoint.requestBody?.content['application/json']
  if (!content) return undefined
  return content.example??schemaExample(spec,content.schema)
}

export async function copyDocumentation(text: string): Promise<void> {
  if(navigator.clipboard && window.isSecureContext) {await navigator.clipboard.writeText(text);return}
  // 现有 HTTP 管理入口不提供 Clipboard API，保留浏览器复制回退。
  const element=document.createElement('textarea');element.value=text;element.style.position='fixed';element.style.opacity='0';element.setAttribute('readonly','');
  const active=document.activeElement as HTMLElement|null
  document.body.appendChild(element);element.select()
  try {if(!document.execCommand('copy')) throw new Error('复制未成功，请选择文本复制')} finally {element.remove();active?.focus()}
}
