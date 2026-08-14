export const MAX_IMAGE_COUNT = 4
export const MAX_IMAGE_BYTES = 4 << 20
export const MAX_IMAGES_TOTAL_BYTES = 4 << 20
export const MAX_CHAT_REQUEST_BYTES = 6 << 20

const supportedTypes = new Set(['image/jpeg', 'image/png', 'image/gif', 'image/webp'])
const strictBase64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/

export function sanitizeImageAttachments(value) {
  if (value === undefined) return []
  if (!Array.isArray(value)) throw new Error('이미지 첨부 형식이 올바르지 않습니다.')
  if (value.length > MAX_IMAGE_COUNT) throw new Error(`이미지는 최대 ${MAX_IMAGE_COUNT}개까지 첨부할 수 있습니다.`)

  let totalBytes = 0
  return value.map((image) => {
    if (!image || typeof image !== 'object' || Array.isArray(image)) {
      throw new Error('이미지 첨부 형식이 올바르지 않습니다.')
    }
    if (typeof image.data !== 'string' || !image.data || image.data.length % 4 !== 0 || !strictBase64.test(image.data)) {
      throw new Error('이미지 데이터가 올바른 Base64 형식이 아닙니다.')
    }
    if (typeof image.mimeType !== 'string' || !supportedTypes.has(image.mimeType)) {
      throw new Error('JPEG, PNG, GIF, WebP 이미지만 첨부할 수 있습니다.')
    }
    if (image.name !== undefined && (typeof image.name !== 'string' || !validFilename(image.name))) {
      throw new Error('첨부 이미지 파일명이 올바르지 않습니다.')
    }

    const decoded = Buffer.from(image.data, 'base64')
    if (!decoded.length || decoded.toString('base64') !== image.data) {
      throw new Error('이미지 데이터가 올바른 Base64 형식이 아닙니다.')
    }
    if (decoded.length > MAX_IMAGE_BYTES) {
      throw new Error(`이미지 한 개의 크기는 ${formatMiB(MAX_IMAGE_BYTES)} 이하여야 합니다.`)
    }
    if (image.size !== undefined && (!Number.isSafeInteger(image.size) || image.size < 0 || image.size !== decoded.length)) {
      throw new Error('첨부 이미지 크기 정보가 실제 데이터와 일치하지 않습니다.')
    }
    if (!validSignature(image.mimeType, decoded)) {
      throw new Error('파일 내용과 이미지 형식이 일치하지 않습니다.')
    }

    totalBytes += decoded.length
    if (totalBytes > MAX_IMAGES_TOTAL_BYTES) {
      throw new Error(`첨부 이미지 전체 크기는 ${formatMiB(MAX_IMAGES_TOTAL_BYTES)} 이하여야 합니다.`)
    }
    return {
      data: image.data,
      mimeType: image.mimeType,
      ...(image.name ? { name: image.name } : {}),
      size: decoded.length,
    }
  })
}

function validFilename(value) {
  return value.length > 0 && value.length <= 255 && !/[\\/\0\r\n]/.test(value)
}

function validSignature(mimeType, data) {
  if (mimeType === 'image/jpeg') return data.length >= 3 && data[0] === 0xff && data[1] === 0xd8 && data[2] === 0xff
  if (mimeType === 'image/png') return data.length >= 8 && data.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]))
  if (mimeType === 'image/gif') return data.length >= 6 && ['GIF87a', 'GIF89a'].includes(data.subarray(0, 6).toString('ascii'))
  if (mimeType === 'image/webp') return data.length >= 12 && data.subarray(0, 4).toString('ascii') === 'RIFF' && data.subarray(8, 12).toString('ascii') === 'WEBP'
  return false
}

function formatMiB(bytes) {
  return `${bytes / (1 << 20)}MiB`
}
