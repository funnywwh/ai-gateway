// The rendered tenant profile must be a document the installed dsh accepts (M68).
//
// What this pins, in the words of the official dsh-llm-pi-ai documentation: the field names
// (`contextWindow`, `maxTokens`, `input`, `reasoningEfforts`), the level vocabulary, the
// modality values, and the shapes dsh's own schemastery schema enforces. The Go golden test
// proves the renderer still produces this document byte for byte; this proves the document
// is *legal*. Together they are what "按官方文档配置 aigw 的模型参数" means in a form a test
// can check.
//
// The stakes are higher than a bad request: dsh refuses an unserviceable `llm-pi-ai` section
// where it is written, so a wrong key or a 0 capacity would leave the tenant's provider
// unregistered and the model menu empty — which is exactly the failure this milestone's
// fields exist to fix.
//
// Run through `make dshgw-test`, which supplies DSHGW_DSH_ROOT (the dsh installation whose
// schema decides acceptance).
import { strict as assert } from 'node:assert'
import { readFile } from 'node:fs/promises'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

const runtime = process.env.DSHGW_DSH_ROOT || '/home/winger/.local/dsh-0.1.2-rc.1'
const goldenPath = process.env.DSHGW_SETTINGS_GOLDEN
  || join(process.cwd(), 'internal/dshgw/tenancy/testdata/aigw-settings.golden.yaml')

const { default: yaml } = await import(pathToFileURL(join(runtime, 'node_modules/js-yaml/dist/js-yaml.mjs')).href)
const { Config } = await import(pathToFileURL(join(runtime, 'node_modules/@deepseek-ai/dsh-llm-pi-ai/lib/index.js')).href)

const document = yaml.load(await readFile(goldenPath, 'utf8'))
assert.ok(document && typeof document === 'object', 'the golden settings must be a YAML mapping')
const section = document['llm-pi-ai']
assert.ok(section, 'the golden settings must carry the llm-pi-ai section the renderer writes')

// Throws, naming the offending path, when the installed dsh cannot serve the section.
const resolved = Config(section)
const provider = resolved.providers?.aigw
assert.ok(provider, 'the aigw route must survive resolution')

// Route facts: the protocol aigw speaks, the endpoint, the strict-mode compat that keeps
// optional tool parameters optional, and the image bound that keeps a request inside aigw's
// body cap (which is why it must survive the schema rather than be dropped as unknown).
assert.equal(provider.api, 'openai-responses')
assert.equal(provider.baseURL, 'http://aigw:8088/v1')
assert.equal(provider.compat.supportsStrictMode, true)
assert.equal(provider.maxRequestImageBytes, 7340032)

const byID = new Map(provider.models.map((model) => [model.id, model]))

// A model with everything disclosed: capacities, image modality, and the seven-level table
// with `off` spelled `none` (the spelling that actually turns thinking off; an empty `off`
// would let the upstream's own default decide).
const flash = byID.get('deepseek-flash')
assert.ok(flash, 'deepseek-flash must survive resolution')
assert.equal(flash.name, 'DeepSeek Flash')
assert.equal(flash.contextWindow, 1000000)
assert.equal(flash.maxTokens, 65536)
assert.deepEqual(flash.input, ['text', 'image'])
assert.deepEqual(flash.reasoningEfforts, {
  off: 'none', minimal: 'minimal', low: 'low', medium: 'medium',
  high: 'high', xhigh: 'xhigh', max: 'max',
})

// Declared non-reasoning: the documented way to state that a model does not reason.
assert.equal(byID.get('plain-model').reasoningEfforts, false)

// Undisclosed facts stay undisclosed: a model aigw said nothing about carries no capacity and
// no reasoning claim, so dsh inherits instead of being told a number nobody measured.
const legacy = byID.get('legacy-model')
assert.equal(legacy.contextWindow, undefined)
assert.equal(legacy.maxTokens, undefined)
assert.equal(legacy.reasoningEfforts, undefined)

// A model whose policy forces an effort offers no menu: the key is omitted rather than
// populated with levels the gateway would override.
assert.equal(byID.get('forced-model').reasoningEfforts, undefined)

// The adapter's documented fallbacks are what an unsized model gets; pinning them here keeps
// the omission above honest (the renderer must not invent a capacity of its own).
assert.equal(provider.defaultContextWindow, 262144)
assert.equal(provider.defaultMaxTokens, 32768)
assert.deepEqual(provider.defaultInput, ['text'])

// The check has teeth: this is the rendering mistake the milestone exists to avoid, and dsh
// refuses it instead of quietly serving a model with no context.
const broken = yaml.load(await readFile(goldenPath, 'utf8'))
broken['llm-pi-ai'].providers.aigw.models[0].contextWindow = 0
assert.throws(() => Config(broken['llm-pi-ai']), /contextWindow/, 'a capacity of 0 must be refused by dsh\'s own schema')

const unknownLevel = yaml.load(await readFile(goldenPath, 'utf8'))
unknownLevel['llm-pi-ai'].providers.aigw.models[0].reasoningEfforts = { extreme: 'extreme' }
assert.throws(() => Config(unknownLevel['llm-pi-ai']), /reasoningEfforts/, 'a level outside the seven must be refused')

console.log(`settings schema check: ${goldenPath} is accepted by ${runtime}`)
