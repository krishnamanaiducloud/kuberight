import { analysisWindow, customRangeError, durationMs, fromLocalInput, rangePresets, toLocalInput } from './timeRange'

const now = Date.UTC(2026, 10, 21, 12, 0, 0)
const iso = (ms: number) => new Date(ms).toISOString()

describe('time ranges', () => {
  it('parses Go durations and offers presets from 15 minutes to 365 days', () => {
    expect(durationMs('15m')).toBe(900_000)
    expect(durationMs('1h30m')).toBe(5_400_000)
    expect(Number.isNaN(durationMs('7d'))).toBe(true)
    const spans = rangePresets.map((preset) => durationMs(preset.value))
    expect(Math.min(...spans)).toBe(900_000)
    expect(Math.max(...spans)).toBe(365 * 24 * 3_600_000)
  })

  it('round-trips datetime-local values through UTC', () => {
    const local = toLocalInput(now)
    expect(fromLocalInput(local)).toBe("2026-11-21T12:00:00Z".replace("12:00", new Date(now).toISOString().slice(11, 16)))
    expect(Date.parse(fromLocalInput(local) ?? "")).toBe(now)
    expect(fromLocalInput('not a date')).toBeUndefined()
  })

  it('validates custom ranges against the API bounds', () => {
    expect(customRangeError({ start: iso(now - 3_600_000), end: iso(now) }, now)).toBeUndefined()
    expect(customRangeError({ start: iso(now - 60_000), end: iso(now) }, now)).toBeUndefined()
    expect(customRangeError({ start: iso(now - 30_000), end: iso(now) }, now)).toMatch(/at least 1 minute/)
    expect(customRangeError({ start: iso(now - 366 * 24 * 3_600_000), end: iso(now) }, now)).toMatch(/365 days/)
    expect(customRangeError({ start: iso(now), end: iso(now - 1) }, now)).toMatch(/after the start/)
    expect(customRangeError({ start: iso(now), end: iso(now + 3_600_000) }, now)).toMatch(/future/)
    expect(customRangeError({ start: '', end: iso(now) }, now)).toMatch(/both/)
  })

  it('clamps analysis windows to the 24h-720h recommendation bounds', () => {
    expect(analysisWindow('15m', undefined, now)).toBe('24h')
    expect(analysisWindow('168h', undefined, now)).toBe('168h')
    expect(analysisWindow('8760h', undefined, now)).toBe('720h')
    expect(analysisWindow('24h', { start: iso(now - 72 * 3_600_000), end: iso(now - 48 * 3_600_000) }, now)).toBe('72h')
  })
})
