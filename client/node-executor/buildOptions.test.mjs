import { test } from "node:test"
import assert from "node:assert/strict"
import { mkdirSync, rmSync, writeFileSync } from "node:fs"
import os from "node:os"
import path from "node:path"
import { randomUUID } from "node:crypto"
import {
  buildOptions,
  resolveCwd,
  ASYNC_EXECUTION_DIRECTIVE,
  claudeChildEnvironment,
  claudeSubscriptionOnlySettings,
  setRuntimeClaudeOAuthTokenForTest,
} from "./executor.mjs"

test("구독 OAuth는 Claude 하위 프로세스에만 전달하고 API 인증은 제거", () => {
  const previous = {
    apiKey: process.env.ANTHROPIC_API_KEY,
    authToken: process.env.ANTHROPIC_AUTH_TOKEN,
    bedrock: process.env.CLAUDE_CODE_USE_BEDROCK,
  }
  process.env.ANTHROPIC_API_KEY = "must-not-pass"
  process.env.ANTHROPIC_AUTH_TOKEN = "must-not-pass"
  process.env.CLAUDE_CODE_USE_BEDROCK = "1"
  setRuntimeClaudeOAuthTokenForTest("sk-ant-oat-unit-test-subscription-token-000000000001")
  try {
    const options = buildOptions({ permissionMode: "default" }, "/tmp/cwd")
    assert.equal(options.env.CLAUDE_CODE_OAUTH_TOKEN, "sk-ant-oat-unit-test-subscription-token-000000000001")
    assert.equal(options.env.CLAUDE_CODE_SUBPROCESS_ENV_SCRUB, "1")
    assert.equal(options.env.ANTHROPIC_API_KEY, "")
    assert.equal(options.env.ANTHROPIC_AUTH_TOKEN, "")
    assert.equal(options.env.CLAUDE_CODE_USE_BEDROCK, "")
    assert.equal(options.env.ANTHROPIC_BASE_URL, "")
    assert.equal(options.env.CLAUDE_CODE_USE_GATEWAY, "")
    assert.equal(options.settings.apiKeyHelper, "")
    assert.equal(options.settings.env.CLAUDE_CODE_SUBPROCESS_ENV_SCRUB, "1")
    assert.equal(options.settings.env.ANTHROPIC_API_KEY, "")
    assert.equal(process.env.CLAUDE_CODE_OAUTH_TOKEN, undefined)
  } finally {
    setRuntimeClaudeOAuthTokenForTest("")
    if (previous.apiKey === undefined) delete process.env.ANTHROPIC_API_KEY
    else process.env.ANTHROPIC_API_KEY = previous.apiKey
    if (previous.authToken === undefined) delete process.env.ANTHROPIC_AUTH_TOKEN
    else process.env.ANTHROPIC_AUTH_TOKEN = previous.authToken
    if (previous.bedrock === undefined) delete process.env.CLAUDE_CODE_USE_BEDROCK
    else process.env.CLAUDE_CODE_USE_BEDROCK = previous.bedrock
  }
})

test("구독 전용 설정은 상위 인증 도우미를 비활성화하고 매번 독립 복사본을 반환", () => {
  const first = claudeSubscriptionOnlySettings()
  const second = claudeSubscriptionOnlySettings()

  assert.notEqual(first, second)
  assert.notEqual(first.env, second.env)
  assert.equal(first.apiKeyHelper, "")
  assert.equal(first.awsCredentialExport, "")
  assert.equal(first.awsAuthRefresh, "")
  assert.equal(first.gcpAuthRefresh, "")
  assert.equal(first.env.CLAUDE_CODE_SUBPROCESS_ENV_SCRUB, "1")

  first.env.ANTHROPIC_API_KEY = "mutated"
  assert.equal(second.env.ANTHROPIC_API_KEY, "")
})

test("구독 OAuth가 없으면 기존 로컬 실행 환경을 건드리지 않음", () => {
  setRuntimeClaudeOAuthTokenForTest("")
  assert.equal(claudeChildEnvironment(), undefined)
  const options = buildOptions({ permissionMode: "default" }, "/tmp/cwd")
  assert.equal(options.env, undefined)
})

test("disallowedTools 미지정 시 options.disallowedTools 없음", () => {
  const options = buildOptions({ permissionMode: "default" }, "/tmp/cwd")
  assert.equal(options.disallowedTools, undefined)
})

test("서브에이전트 본문과 주기적 진행 요약을 실시간 전달", () => {
  const options = buildOptions({ permissionMode: "default" }, "/tmp/cwd")
  assert.equal(options.includePartialMessages, true)
  assert.equal(options.forwardSubagentText, true)
  assert.equal(options.agentProgressSummaries, true)
})

test("disallowedTools 지정 시 그대로 전달", () => {
  const denylist = ["Bash(rm -rf *)", "Bash(git push*)"]
  const options = buildOptions(
    { permissionMode: "bypassPermissions", disallowedTools: denylist },
    "/tmp/cwd",
  )
  assert.deepEqual(options.disallowedTools, denylist)
})

test("빈 배열이면 options.disallowedTools도 빈 배열", () => {
  const options = buildOptions(
    { permissionMode: "bypassPermissions", disallowedTools: [] },
    "/tmp/cwd",
  )
  assert.deepEqual(options.disallowedTools, [])
})

test("객체 형태면 명확한 에러를 던진다 (silent-vanish 방지)", () => {
  assert.throws(
    () =>
      buildOptions(
        { permissionMode: "bypassPermissions", disallowedTools: { denied: ["Bash"] } },
        "/tmp/cwd",
      ),
    /invalid disallowedTools: must be an array of strings/,
  )
})

test("문자열 형태면 명확한 에러를 던진다 (SDK 내부 변수명 유출 방지)", () => {
  assert.throws(
    () =>
      buildOptions(
        { permissionMode: "bypassPermissions", disallowedTools: "Bash" },
        "/tmp/cwd",
      ),
    /invalid disallowedTools: must be an array of strings/,
  )
})

test("배열이지만 문자열이 아닌 원소가 섞이면 에러를 던진다", () => {
  assert.throws(
    () =>
      buildOptions(
        { permissionMode: "bypassPermissions", disallowedTools: ["Bash", 123] },
        "/tmp/cwd",
      ),
    /invalid disallowedTools: must be an array of strings/,
  )
})

test("systemPrompt 미지정 시 기본 claude_code 프리셋(append 포함) 유지", () => {
  const options = buildOptions({ permissionMode: "default" }, "/tmp/cwd")
  assert.deepEqual(options.systemPrompt, {
    type: "preset",
    preset: "claude_code",
    append: ASYNC_EXECUTION_DIRECTIVE,
  })
  assert.match(options.systemPrompt.append, /Relay가 인증된 참가자/)
})

test("systemPrompt 문자열 지정 시 완전히 대체(프리셋/append 없이 그 문자열 그대로)", () => {
  const custom = "당신은 텍스트 포맷터입니다. 지시를 그대로 따르세요."
  const options = buildOptions(
    { permissionMode: "bypassPermissions", systemPrompt: custom },
    "/tmp/cwd",
  )
  assert.equal(options.systemPrompt, custom)
})

test("systemPrompt 빈 문자열이면 미지정으로 취급해 기본 프리셋 유지", () => {
  const options = buildOptions(
    { permissionMode: "default", systemPrompt: "" },
    "/tmp/cwd",
  )
  assert.deepEqual(options.systemPrompt, {
    type: "preset",
    preset: "claude_code",
    append: ASYNC_EXECUTION_DIRECTIVE,
  })
})

test("systemPrompt 문자열이 아니면 명확한 에러를 던진다 (disallowedTools와 동일한 fail-closed 원칙)", () => {
  assert.throws(
    () => buildOptions({ permissionMode: "default", systemPrompt: 123 }, "/tmp/cwd"),
    /invalid systemPrompt: must be a string/,
  )
  assert.throws(
    () => buildOptions({ permissionMode: "default", systemPrompt: { foo: "bar" } }, "/tmp/cwd"),
    /invalid systemPrompt: must be a string/,
  )
})

test("disallowedTools + claudePath + extendedThinking + resumable sessionId 병행 시 서로 덮어쓰지 않음", () => {
  const cwd = "/tmp/build-options-combo-test"
  const sessionId = randomUUID()
  const base = path.join(os.homedir(), ".claude", "projects")
  const dir = path.join(base, cwd.replace(/\//g, "-"))
  const jsonlFile = path.join(dir, `${sessionId}.jsonl`)
  mkdirSync(dir, { recursive: true })
  writeFileSync(jsonlFile, "")

  try {
    const denylist = ["Bash(rm -rf *)"]
    const options = buildOptions(
      {
        permissionMode: "bypassPermissions",
        disallowedTools: denylist,
        claudePath: "/opt/custom/claude",
        extendedThinking: true,
        sessionId,
      },
      cwd,
    )

    assert.deepEqual(options.disallowedTools, denylist)
    assert.equal(options.pathToClaudeCodeExecutable, "/opt/custom/claude")
    assert.deepEqual(options.thinking, { type: "enabled", budget_tokens: 10000 })
    assert.equal(options.resume, sessionId)
  } finally {
    rmSync(jsonlFile, { force: true })
  }
})

// ── P3-2 방 권한 정책: CLI_RELAY_PERMISSION_MODE ──────────────────

test("CLI_RELAY_PERMISSION_MODE 미설정 시 req.permissionMode 그대로 사용", () => {
  delete process.env.CLI_RELAY_PERMISSION_MODE
  const options = buildOptions({ permissionMode: "acceptEdits" }, "/tmp/cwd")
  assert.equal(options.permissionMode, "acceptEdits")
})

test("CLI_RELAY_PERMISSION_MODE 설정 시 req.permissionMode 를 무시하고 env 값 사용", () => {
  const prev = process.env.CLI_RELAY_PERMISSION_MODE
  process.env.CLI_RELAY_PERMISSION_MODE = "plan"
  try {
    const options = buildOptions({ permissionMode: "bypassPermissions" }, "/tmp/cwd")
    assert.equal(options.permissionMode, "plan")
  } finally {
    if (prev === undefined) delete process.env.CLI_RELAY_PERMISSION_MODE
    else process.env.CLI_RELAY_PERMISSION_MODE = prev
  }
})

test("CLI_RELAY_PERMISSION_MODE 허용값을 적용하고 관리형 bypass는 자동 승인 경로로 변환", () => {
  const prev = process.env.CLI_RELAY_PERMISSION_MODE
  try {
    for (const mode of ["default", "acceptEdits", "plan", "bypassPermissions"]) {
      process.env.CLI_RELAY_PERMISSION_MODE = mode
      const options = buildOptions({ permissionMode: "default" }, "/tmp/cwd")
      if (mode === "bypassPermissions") {
        assert.equal(options.permissionMode, "default")
        assert.equal(options.allowDangerouslySkipPermissions, undefined)
        assert.equal(options.extraArgs, undefined)
        assert.equal(typeof options.canUseTool, "function")
      } else {
        assert.equal(options.permissionMode, mode)
        assert.equal(options.allowDangerouslySkipPermissions, undefined)
        assert.equal(options.extraArgs, undefined)
        assert.equal(typeof options.canUseTool, "function")
      }
    }
  } finally {
    if (prev === undefined) delete process.env.CLI_RELAY_PERMISSION_MODE
    else process.env.CLI_RELAY_PERMISSION_MODE = prev
  }
})

test("Manager가 고정한 bypassPermissions는 UI 요청 없이 도구 입력을 자동 승인", async () => {
  const prev = process.env.CLI_RELAY_PERMISSION_MODE
  process.env.CLI_RELAY_PERMISSION_MODE = "bypassPermissions"
  try {
    const input = { file_path: "/workspace/example.txt", content: "ok" }
    const options = buildOptions({ permissionMode: "plan" }, "/tmp/cwd")
    assert.deepEqual(await options.canUseTool("Write", input), {
      behavior: "allow",
      updatedInput: input,
    })
  } finally {
    if (prev === undefined) delete process.env.CLI_RELAY_PERMISSION_MODE
    else process.env.CLI_RELAY_PERMISSION_MODE = prev
  }
})

test("요청에서만 선택한 bypassPermissions는 SDK 네이티브 우회 경로 유지", () => {
  const prev = process.env.CLI_RELAY_PERMISSION_MODE
  delete process.env.CLI_RELAY_PERMISSION_MODE
  try {
    const options = buildOptions({ permissionMode: "bypassPermissions" }, "/tmp/cwd")
    assert.equal(options.permissionMode, "bypassPermissions")
    assert.equal(options.allowDangerouslySkipPermissions, true)
    assert.deepEqual(options.extraArgs, { "dangerously-skip-permissions": null })
    assert.equal(options.canUseTool, undefined)
  } finally {
    if (prev === undefined) delete process.env.CLI_RELAY_PERMISSION_MODE
    else process.env.CLI_RELAY_PERMISSION_MODE = prev
  }
})

test("CLI_RELAY_PERMISSION_MODE 가 허용값 밖이면 명확한 에러를 던진다 (fail loudly)", () => {
  const prev = process.env.CLI_RELAY_PERMISSION_MODE
  process.env.CLI_RELAY_PERMISSION_MODE = "yolo"
  try {
    assert.throws(
      () => buildOptions({ permissionMode: "default" }, "/tmp/cwd"),
      /invalid CLI_RELAY_PERMISSION_MODE: yolo/,
    )
  } finally {
    if (prev === undefined) delete process.env.CLI_RELAY_PERMISSION_MODE
    else process.env.CLI_RELAY_PERMISSION_MODE = prev
  }
})

test("CLI_RELAY_PERMISSION_MODE 빈 문자열이면 미설정으로 취급해 req.permissionMode 사용", () => {
  const prev = process.env.CLI_RELAY_PERMISSION_MODE
  process.env.CLI_RELAY_PERMISSION_MODE = ""
  try {
    const options = buildOptions({ permissionMode: "acceptEdits" }, "/tmp/cwd")
    assert.equal(options.permissionMode, "acceptEdits")
  } finally {
    if (prev === undefined) delete process.env.CLI_RELAY_PERMISSION_MODE
    else process.env.CLI_RELAY_PERMISSION_MODE = prev
  }
})

// ── P3-2 방 권한 정책: CLI_RELAY_DEFAULT_CWD (resolveCwd) ─────────

test("req.cwd 가 존재하는 경로면 CLI_RELAY_DEFAULT_CWD 무시하고 그대로 사용", () => {
  const prev = process.env.CLI_RELAY_DEFAULT_CWD
  process.env.CLI_RELAY_DEFAULT_CWD = os.homedir()
  try {
    const cwd = resolveCwd({ cwd: "/tmp" })
    assert.equal(cwd, "/tmp")
  } finally {
    if (prev === undefined) delete process.env.CLI_RELAY_DEFAULT_CWD
    else process.env.CLI_RELAY_DEFAULT_CWD = prev
  }
})

test("req.cwd 없고 CLI_RELAY_DEFAULT_CWD 가 존재하는 경로면 homedir 대신 그 경로 사용", () => {
  const prev = process.env.CLI_RELAY_DEFAULT_CWD
  const dir = path.join(os.tmpdir(), `resolve-cwd-default-${randomUUID()}`)
  mkdirSync(dir, { recursive: true })
  process.env.CLI_RELAY_DEFAULT_CWD = dir
  try {
    const cwd = resolveCwd({})
    assert.equal(cwd, dir)
  } finally {
    if (prev === undefined) delete process.env.CLI_RELAY_DEFAULT_CWD
    else process.env.CLI_RELAY_DEFAULT_CWD = prev
    rmSync(dir, { recursive: true, force: true })
  }
})

test("req.cwd 가 존재하지 않는 경로이고 CLI_RELAY_DEFAULT_CWD 가 존재하면 그 경로로 폴백", () => {
  const prev = process.env.CLI_RELAY_DEFAULT_CWD
  const dir = path.join(os.tmpdir(), `resolve-cwd-default-${randomUUID()}`)
  mkdirSync(dir, { recursive: true })
  process.env.CLI_RELAY_DEFAULT_CWD = dir
  try {
    const cwd = resolveCwd({ cwd: "/no/such/path/at/all" })
    assert.equal(cwd, dir)
  } finally {
    if (prev === undefined) delete process.env.CLI_RELAY_DEFAULT_CWD
    else process.env.CLI_RELAY_DEFAULT_CWD = prev
    rmSync(dir, { recursive: true, force: true })
  }
})

test("CLI_RELAY_DEFAULT_CWD 도 존재하지 않으면 homedir 로 폴백 (기존 동작 유지)", () => {
  const prev = process.env.CLI_RELAY_DEFAULT_CWD
  process.env.CLI_RELAY_DEFAULT_CWD = "/no/such/default/cwd/at/all"
  try {
    const cwd = resolveCwd({})
    assert.equal(cwd, os.homedir())
  } finally {
    if (prev === undefined) delete process.env.CLI_RELAY_DEFAULT_CWD
    else process.env.CLI_RELAY_DEFAULT_CWD = prev
  }
})

test("CLI_RELAY_DEFAULT_CWD 미설정이면 기존 동작대로 homedir 로 폴백", () => {
  const prev = process.env.CLI_RELAY_DEFAULT_CWD
  delete process.env.CLI_RELAY_DEFAULT_CWD
  try {
    const cwd = resolveCwd({})
    assert.equal(cwd, os.homedir())
  } finally {
    if (prev === undefined) delete process.env.CLI_RELAY_DEFAULT_CWD
    else process.env.CLI_RELAY_DEFAULT_CWD = prev
  }
})
