#!/usr/bin/env node

import { hashPassword } from '../src/auth.mjs'

const password = process.argv[2]
if (!password) {
  console.error('Usage: npm run hash-password -- <password>')
  process.exit(2)
}
console.log(await hashPassword(password))
