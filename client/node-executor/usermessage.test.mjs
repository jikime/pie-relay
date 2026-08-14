import { test } from "node:test"
import assert from "node:assert/strict"
import { userMessage, validateImages, speakerPrefixed } from "./executor.mjs"

test("images 없이 호출하면 텍스트 블록 하나만 생성", () => {
  const msg = userMessage("안녕", undefined)
  assert.deepEqual(msg.message.content, [{ type: "text", text: "안녕" }])
  assert.equal(msg.type, "user")
  assert.equal(msg.message.role, "user")
  assert.equal(msg.parent_tool_use_id, null)
})

test("images 가 있으면 이미지 블록들이 텍스트 앞에 온다", () => {
  const images = [
    { data: "aGVsbG8=", mimeType: "image/png" },
    { data: "d29ybGQ=", mimeType: "image/jpeg" },
  ]
  const msg = userMessage("분석해줘", images)
  assert.deepEqual(msg.message.content, [
    { type: "image", source: { type: "base64", media_type: "image/png", data: "aGVsbG8=" } },
    { type: "image", source: { type: "base64", media_type: "image/jpeg", data: "d29ybGQ=" } },
    { type: "text", text: "분석해줘" },
  ])
})

test("images 가 빈 배열이면 텍스트 블록만 생성(에러 아님)", () => {
  const msg = userMessage("안녕", [])
  assert.deepEqual(msg.message.content, [{ type: "text", text: "안녕" }])
})

test("validateImages: undefined 는 유효(이미지 없는 기존 호출)", () => {
  assert.equal(validateImages(undefined), true)
})

test("validateImages: 올바른 배열은 유효", () => {
  assert.equal(
    validateImages([{ data: "aGVsbG8=", mimeType: "image/png" }]),
    true,
  )
})

test("validateImages: 배열이 아니면 무효", () => {
  assert.equal(validateImages("not an array"), false)
  assert.equal(validateImages({ data: "x", mimeType: "image/png" }), false)
})

test("validateImages: 원소에 data/mimeType 문자열이 없으면 무효", () => {
  assert.equal(validateImages([{ data: 123, mimeType: "image/png" }]), false)
  assert.equal(validateImages([{ data: "x" }]), false)
  assert.equal(validateImages([{}]), false)
  assert.equal(validateImages([null]), false)
})

test("validateImages: 빈 배열은 유효", () => {
  assert.equal(validateImages([]), true)
})

test("userMessage: images 가 null 이면 텍스트 블록만 생성(크래시 없음)", () => {
  const msg = userMessage("안녕", null)
  assert.deepEqual(msg.message.content, [{ type: "text", text: "안녕" }])
})

test("validateImages: 이미지 개수가 상한을 넘으면 무효", () => {
  const images = Array.from({ length: 21 }, () => ({ data: "aGVsbG8=", mimeType: "image/png" }))
  assert.equal(validateImages(images), false)
  assert.equal(validateImages(images.slice(0, 20)), true)
})

test("validateImages: 이미지 데이터가 상한 크기를 넘으면 무효", () => {
  const tooBig = "a".repeat(15 * 1024 * 1024 + 1)
  assert.equal(validateImages([{ data: tooBig, mimeType: "image/png" }]), false)
})

test("validateImages: 빈 문자열 data/mimeType 은 무효", () => {
  assert.equal(validateImages([{ data: "", mimeType: "" }]), false)
})

test("validateImages: 지원하지 않는 mimeType 은 무효", () => {
  assert.equal(validateImages([{ data: "aGVsbG8=", mimeType: "image/bmp" }]), false)
  assert.equal(validateImages([{ data: "aGVsbG8=", mimeType: "not-a-mime-type" }]), false)
})

test("speakerPrefixed: from 없으면 원문 그대로(하위호환)", () => {
  assert.equal(speakerPrefixed("이 함수 설명해줘", undefined), "이 함수 설명해줘")
})

test("speakerPrefixed: from 이 문자열이 아니면 원문 그대로", () => {
  assert.equal(speakerPrefixed("이 함수 설명해줘", 123), "이 함수 설명해줘")
  assert.equal(speakerPrefixed("이 함수 설명해줘", { sub: "bob" }), "이 함수 설명해줘")
})

test("speakerPrefixed: from 이 빈 문자열이면 원문 그대로", () => {
  assert.equal(speakerPrefixed("이 함수 설명해줘", ""), "이 함수 설명해줘")
})

test("speakerPrefixed: guest:<name>-<rand> 형식이면 이름만 표기", () => {
  assert.equal(
    speakerPrefixed("이 함수 설명해줘", "guest:bob-x7k2"),
    "[bob] 이 함수 설명해줘",
  )
})

test("speakerPrefixed: guest 형식이 아니면 from 전체를 표기", () => {
  assert.equal(speakerPrefixed("안녕", "alice@example.com"), "[alice@example.com] 안녕")
  assert.equal(speakerPrefixed("안녕", "host-sub-1234"), "[host-sub-1234] 안녕")
})
