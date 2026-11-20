// @vitest-environment node
// Checks the files scripts/brand-assets.mjs generates, and every reference
// to them, so a missing or malformed icon fails the build instead of the tab.
import { existsSync, readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

const web = resolve(import.meta.dirname, '..')
const read = (name) => readFileSync(resolve(web, 'public', name))

function pngSize(name) {
  const png = read(name)
  expect(png.subarray(0, 8).toString('hex'), `${name} is a PNG`).toBe('89504e470d0a1a0a')
  return { width: png.readUInt32BE(16), height: png.readUInt32BE(20) }
}

describe('KubeRight brand assets', () => {
  it('ships every favicon size the browsers and home screens ask for', () => {
    expect(pngSize('favicon-16x16.png')).toEqual({ width: 16, height: 16 })
    expect(pngSize('favicon-32x32.png')).toEqual({ width: 32, height: 32 })
    expect(pngSize('apple-touch-icon.png')).toEqual({ width: 180, height: 180 })
    expect(pngSize('android-chrome-192x192.png')).toEqual({ width: 192, height: 192 })
    expect(pngSize('android-chrome-512x512.png')).toEqual({ width: 512, height: 512 })
  })

  it('packs 16, 32, and 48 pixel images into favicon.ico', () => {
    const ico = read('favicon.ico')
    expect(ico.readUInt16LE(0)).toBe(0)
    expect(ico.readUInt16LE(2)).toBe(1)
    const count = ico.readUInt16LE(4)
    const sizes = Array.from({ length: count }, (_, index) => ico.readUInt8(6 + 16 * index))
    expect(sizes).toEqual([16, 32, 48])
    for (let index = 0; index < count; index++) {
      const length = ico.readUInt32LE(6 + 16 * index + 8)
      const offset = ico.readUInt32LE(6 + 16 * index + 12)
      expect(offset + length).toBeLessThanOrEqual(ico.length)
      expect(ico.subarray(offset, offset + 8).toString('hex')).toBe('89504e470d0a1a0a')
    }
  })

  it('uses the original KubeRight mark, not a Kubernetes logo', () => {
    const svg = read('favicon.svg').toString('utf8')
    expect(svg).toMatch(/^<svg xmlns="http:\/\/www\.w3\.org\/2000\/svg"/)
    expect(svg).toContain('<title>KubeRight</title>')
    expect(svg).not.toMatch(/kubernetes/i)
  })

  it('references only icons that exist, relative to the base URL', () => {
    const manifest = JSON.parse(read('site.webmanifest').toString('utf8'))
    expect(manifest.name).toBe('KubeRight | Kubernetes Resource Intelligence')
    expect(manifest.short_name).toBe('KubeRight')
    expect(manifest.start_url).toBe('.')
    for (const icon of manifest.icons) {
      expect(icon.src.startsWith('/'), `${icon.src} must be relative so a URL prefix works`).toBe(false)
      expect(existsSync(resolve(web, 'public', icon.src)), icon.src).toBe(true)
    }
    const html = readFileSync(resolve(web, 'index.html'), 'utf8')
    expect(html).toContain('<title>KubeRight | Kubernetes Resource Intelligence</title>')
    expect(html.indexOf('<base href="/" />')).toBeLessThan(html.indexOf('<link '))
    const linked = [...html.matchAll(/<link rel="(?:icon|apple-touch-icon|manifest)" href="\/([^"]+)"/g)].map((match) => match[1])
    expect(linked.sort()).toEqual(['apple-touch-icon.png', 'favicon-16x16.png', 'favicon-32x32.png', 'favicon.ico', 'favicon.svg', 'site.webmanifest'])
    for (const file of linked) expect(existsSync(resolve(web, 'public', file)), file).toBe(true)
  })
})
