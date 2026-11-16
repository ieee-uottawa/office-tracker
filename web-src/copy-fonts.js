// Copies the Geist variable fonts (the same files the website loads through
// the `geist` package) next to the compiled stylesheet.
const fs = require('fs')
const path = require('path')
const out = path.join(__dirname, '../internal/transport/http/web/assets/fonts')
fs.mkdirSync(out, { recursive: true })
for (const [dir, file] of [['geist-sans', 'Geist-Variable.woff2'], ['geist-mono', 'GeistMono-Variable.woff2']]) {
  fs.copyFileSync(path.join(__dirname, 'node_modules/geist/dist/fonts', dir, file), path.join(out, file))
}
fs.copyFileSync(path.join(__dirname, 'node_modules/geist/LICENSE.txt'), path.join(out, 'Geist-LICENSE.txt'))
