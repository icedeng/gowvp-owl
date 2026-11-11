<script setup lang="ts">
import { createRequestKey } from '../requestKey'
import { computed, ref, shallowRef } from 'vue'
import ModalDialog from '../../../components/ModalDialog.vue'
import { errorMessage } from '../../../services/api'
import { gatewayAPI } from '../api'
import type { Migration, Subject } from '../types'
import GatewaySelector from './GatewaySelector.vue'
import { useVisiblePoll } from '../composables/useVisiblePoll'
const props = defineProps<{ source: string | null; subjects: Subject[]; label?: string }>()
const open = ref(false); const target = ref(''); const busy = ref(false); const error = ref('')
const migration = shallowRef<Migration>(); const migrationID = ref(''); const page = ref(1)
const expiredAt = ref(''); let requestKey = ''; let lastBody = ''
const active = computed(() => migration.value && !['completed','expired','cancelled'].includes(migration.value.state))
const { refresh } = useVisiblePoll(async signal => {
  if (!migrationID.value) return
  try { const result = await gatewayAPI.migration(migrationID.value,page.value,signal); if(!signal.aborted) {migration.value=result;error.value=''} }
  catch(e) {if(!signal.aborted) error.value=errorMessage(e)}
})
function begin() { open.value=true; target.value=''; expiredAt.value=new Date(Date.now()+3600000).toISOString();error.value='' }
async function create() {
  busy.value=true;error.value=''
  try {
    const body={source:props.source,target:target.value||null,subjects:props.subjects,expires_at:expiredAt.value}
    const fingerprint=JSON.stringify(body)
    if(fingerprint!==lastBody){lastBody=fingerprint;requestKey=createRequestKey()}
    const op=await gatewayAPI.migrateSubjects(body.source,body.target,body.subjects,body.expires_at,requestKey)
    const id=(op.result as {migration_id?:string}|null)?.migration_id
    if(!id) throw new Error('操作未返回迁移计划 ID')
    migrationID.value=id;page.value=1;open.value=false;await refresh()
  }catch(e){error.value=errorMessage(e)}finally{busy.value=false}
}
async function cancel() {
  if(!migration.value)return
  busy.value=true
  try{await gatewayAPI.cancelMigration(migration.value,'管理员取消尚未提交的迁移对象',createRequestKey());await refresh()}
  catch(e){error.value=errorMessage(e)}finally{busy.value=false}
}
</script>
<template><div class="migration-panel"><button type="button" class="btn btn-sm" :disabled="!subjects.length || busy || !!active" @click="begin">{{ label || '迁移入口' }}</button><p v-if="error" role="alert">{{ error }}</p><div v-if="migration"><p>迁移 {{ migration.id }}：{{ migration.state }}</p><ul><li v-for="item in migration.items" :key="item.subject_id">{{ item.subject_id }} · {{ item.state }} {{ item.error?.message }}</li></ul><div class="actions"><button type="button" class="btn btn-sm" :disabled="page===1" @click="page--;refresh()">上一批</button><button type="button" class="btn btn-sm" :disabled="migration.items.length<migration.size" @click="page++;refresh()">下一批</button><button v-if="active" type="button" class="btn btn-sm" :disabled="busy" @click="cancel">取消剩余迁移</button></div></div><ModalDialog :open="open" title="创建入口迁移计划" @close="!busy && (open=false)"><p>选择 {{ subjects.length }} 个对象的一小时迁移目标。原入口先排空，目标通过新注册认证后提交。请同步调整设备地址；上级平台还需保存新的平台入口配置。</p><GatewaySelector v-model="target" label="迁移目标" /><p v-if="(target||null)===source">请选择与当前入口不同的目标。</p><template #footer><button type="button" class="btn" :disabled="busy" @click="open=false">取消</button><button type="button" class="btn btn-primary" :disabled="busy || !subjects.length || (target||null)===source" @click="create">{{ busy ? '提交中…' : '创建计划' }}</button></template></ModalDialog></div></template>
<style scoped>.migration-panel{margin:8px 0}.actions{display:flex;gap:8px;flex-wrap:wrap}.migration-panel p{margin:8px 0;overflow-wrap:anywhere}.migration-panel ul{padding-left:20px}</style>
