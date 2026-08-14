import assert from 'node:assert/strict'
import { test } from 'node:test'

import { MAX_IMAGE_COUNT, sanitizeImageAttachments } from '../src/image-attachments.mjs'

const png = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII='

test('image attachments are normalized without mutating the caller', () => {
  const input = [{ data: png, mimeType: 'image/png', name: 'pixel.png', size: 68 }]
  const result = sanitizeImageAttachments(input)
  assert.notEqual(result, input)
  assert.notEqual(result[0], input[0])
  assert.deepEqual(result, input)
  assert.deepEqual(sanitizeImageAttachments(undefined), [])
})

test('image attachments reject spoofed content, unsafe names, and inconsistent sizes', () => {
  assert.throws(
    () => sanitizeImageAttachments([{ data: Buffer.from('not an image').toString('base64'), mimeType: 'image/png' }]),
    /파일 내용과 이미지 형식/,
  )
  assert.throws(
    () => sanitizeImageAttachments([{ data: png, mimeType: 'image/png', name: '../pixel.png' }]),
    /파일명/,
  )
  assert.throws(
    () => sanitizeImageAttachments([{ data: png, mimeType: 'image/png', size: 1 }]),
    /크기 정보/,
  )
  assert.throws(
    () => sanitizeImageAttachments(Array.from({ length: MAX_IMAGE_COUNT + 1 }, () => ({ data: png, mimeType: 'image/png' }))),
    /최대 4개/,
  )
})

test('image attachments require canonical padded Base64', () => {
  assert.throws(() => sanitizeImageAttachments([{ data: `${png}\n`, mimeType: 'image/png' }]), /Base64/)
  assert.throws(() => sanitizeImageAttachments([{ data: png.replace(/=$/, ''), mimeType: 'image/png' }]), /Base64/)
})
