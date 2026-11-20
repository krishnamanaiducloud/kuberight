// Renders the KubeRight brand mark (public/favicon.svg) into the PNG and ICO
// favicon set with the Playwright Chromium the end-to-end tests already use.
// Run after changing the mark: npm run brand:assets
import { readFile, writeFile } from 'node:fs/promises'
import { chromium } from '@playwright/test'

const publicDir = new URL('../public/', import.meta.url)
const svg = await readFile(new URL('favicon.svg', publicDir), 'utf8')
// Home-screen icons are shown without transparency, so they get a square tile.
const squareSvg = svg.replace('rx="14"', 'rx="0"')

const browser = await chromium.launch()
const page = await browser.newPage({ deviceScaleFactor: 1 })

async function render(size, source = svg) {
  await page.setViewportSize({ width: size, height: size })
  const sized = source.replace('<svg ', `<svg width="${size}" height="${size}" `)
  await page.setContent(`<!doctype html><html><body style="margin:0;background:transparent">${sized}</body></html>`)
  return page.screenshot({ omitBackground: true, clip: { x: 0, y: 0, width: size, height: size } })
}

// An ICO file whose images are PNGs, which every current browser and
// Windows since Vista read.
function ico(images) {
  const header = Buffer.alloc(6 + 16 * images.length)
  header.writeUInt16LE(0, 0)
  header.writeUInt16LE(1, 2)
  header.writeUInt16LE(images.length, 4)
  let offset = header.length
  images.forEach(({ size, png }, index) => {
    const entry = 6 + 16 * index
    header.writeUInt8(size >= 256 ? 0 : size, entry)
    header.writeUInt8(size >= 256 ? 0 : size, entry + 1)
    header.writeUInt8(0, entry + 2)
    header.writeUInt8(0, entry + 3)
    header.writeUInt16LE(1, entry + 4)
    header.writeUInt16LE(32, entry + 6)
    header.writeUInt32LE(png.length, entry + 8)
    header.writeUInt32LE(offset, entry + 12)
    offset += png.length
  })
  return Buffer.concat([header, ...images.map(({ png }) => png)])
}

const outputs = {
  'favicon-16x16.png': await render(16),
  'favicon-32x32.png': await render(32),
  'apple-touch-icon.png': await render(180, squareSvg),
  'android-chrome-192x192.png': await render(192),
  'android-chrome-512x512.png': await render(512),
}
outputs['favicon.ico'] = ico([
  { size: 16, png: outputs['favicon-16x16.png'] },
  { size: 32, png: outputs['favicon-32x32.png'] },
  { size: 48, png: await render(48) },
])
await browser.close()

for (const [name, data] of Object.entries(outputs)) {
  await writeFile(new URL(name, publicDir), data)
  console.log(`${name}: ${data.length} bytes`)
}
