
// @deno-types="https://esm.sh/v127/@astrojs/compiler@1.5.3/dist/browser/index.d.ts"
import * as Compiler from "./packages/compiler/dist/browser/index.js"
const wasmURL = import.meta.resolve("./packages/compiler/dist/astro.wasm")
await Compiler.initialize({ wasmURL })

const source = `
---
function f() {}
---
<script define:serverfunctions={{ f }}> f() </script>
`

const result = await Compiler.transform(source, { filename: "x.ts" })

// console.log("result", result)
console.log(result.code)