<script setup lang="ts">
import { ref } from 'vue'
import { copyDocumentation } from '../services/open-platform-docs'
const props=defineProps<{code:string;language?:string}>()
const message=ref('')
async function copy(){try{await copyDocumentation(props.code);message.value='已复制'}catch{message.value='请选择下方文本复制'}}
</script>
<template><div class="docs-code"><div class="code-head"><span>{{ language || 'text' }}</span><button type="button" @click="copy">{{ message || '复制' }}</button><span class="sr-only" role="status">{{ message }}</span></div><pre><code>{{ code }}</code></pre></div></template>
<style scoped>
.docs-code{border:1px solid #dce2ea;border-radius:9px;overflow:hidden;margin:16px 0;background:#f7f9fc}.code-head{display:flex;align-items:center;justify-content:space-between;padding:8px 14px;border-bottom:1px solid #e1e7ef;color:#53637a;font-size:12px}.code-head button{padding:4px 8px;border:1px solid #cad4e3;border-radius:5px;background:white;color:#254c83}.docs-code pre{margin:0;padding:16px;overflow:auto;font:13px/1.8 ui-monospace,SFMono-Regular,Consolas,monospace;color:#1a3151;tab-size:2}
</style>
