import { defineConfig } from 'vitest/config'
import { resolve } from 'node:path'

export default defineConfig({
  define: { __DEV__: false },
  resolve: {
    alias: {
      'react-native': resolve(__dirname, 'test/react-native-stub.ts')
      , 'expo-crypto': resolve(__dirname, 'test/expo-crypto-stub.ts')
    }
  }
})
