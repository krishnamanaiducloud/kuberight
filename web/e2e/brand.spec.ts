import { expect, test } from '@playwright/test'

const icons: Record<string, RegExp> = {
  'favicon.ico': /^image\/(x-icon|vnd\.microsoft\.icon)/,
  'favicon.svg': /^image\/svg\+xml/,
  'favicon-16x16.png': /^image\/png/,
  'favicon-32x32.png': /^image\/png/,
  'apple-touch-icon.png': /^image\/png/,
  'android-chrome-192x192.png': /^image\/png/,
  'android-chrome-512x512.png': /^image\/png/,
}

test('shows the KubeRight title, icon, and mark without missing assets', async ({ page }) => {
  const missing: string[] = []
  page.on('response', (response) => {
    if (response.status() >= 400 && !new URL(response.url()).pathname.startsWith('/api/')) missing.push(`${response.status()} ${response.url()}`)
  })
  await page.route('**/api/v1/**', (route) => route.fulfill({ json: { items: [], mode: 'recommend' } }))
  await page.goto('/overview')
  await expect(page).toHaveTitle('KubeRight | Kubernetes Resource Intelligence')

  const links = await page.locator('link[rel="icon"], link[rel="apple-touch-icon"], link[rel="manifest"]').evaluateAll((elements) =>
    elements.map((element) => (element as HTMLLinkElement).href))
  expect(links.length).toBe(6)
  for (const href of links) {
    const response = await page.request.get(href)
    expect(response.status(), href).toBe(200)
  }
  for (const [file, type] of Object.entries(icons)) {
    const response = await page.request.get(`/${file}`)
    expect(response.status(), file).toBe(200)
    expect(response.headers()['content-type'], file).toMatch(type)
  }

  // The sidebar carries the same mark as the browser tab.
  const mark = page.locator('img[src="favicon.svg"]').first()
  await expect(mark).toBeAttached()
  expect(await mark.evaluate((image: HTMLImageElement) => image.complete && image.naturalWidth > 0)).toBe(true)
  expect(missing).toEqual([])
})
