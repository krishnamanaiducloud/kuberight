/**
 * Usage chart ranges: a preset lookback ending now, or a custom start and end.
 * The API evaluates every range in UTC; the dashboard shows and edits times
 * in the browser's own time zone.
 */
export type CustomRange = { start: string; end: string }

export const minimumRangeMs = 60_000
export const maximumRangeMs = 365 * 24 * 3_600_000

/** Preset lookbacks for usage charts. Values are Go durations. */
export const rangePresets = [
  { value: '15m', label: '15 minutes' },
  { value: '1h', label: '1 hour' },
  { value: '6h', label: '6 hours' },
  { value: '24h', label: '24 hours' },
  { value: '72h', label: '3 days' },
  { value: '168h', label: '7 days' },
  { value: '720h', label: '30 days' },
  { value: '2160h', label: '90 days' },
  { value: '8760h', label: '365 days' },
]

const units: Record<string, number> = { s: 1_000, m: 60_000, h: 3_600_000 }

/** Milliseconds in a Go duration made of whole s, m, and h units, or NaN. */
export function durationMs(value: string): number {
  const parts = [...value.matchAll(/(\d+)([smh])/g)]
  if (parts.length === 0 || parts.map((part) => part[0]).join('') !== value) return Number.NaN
  return parts.reduce((total, [, amount, unit]) => total + Number(amount) * units[unit], 0)
}

/** `datetime-local` value (browser time zone) for an instant. */
export function toLocalInput(iso: string | number | Date): string {
  const date = new Date(iso)
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

/** The UTC instant of a `datetime-local` value, read in the browser time zone. */
export function fromLocalInput(value: string): string | undefined {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$/.test(value)) return undefined
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString().replace(/\.\d{3}Z$/, 'Z')
}

/** Why a custom range is invalid, or undefined when the API will accept it. */
export function customRangeError(range: CustomRange, now = Date.now()): string | undefined {
  const start = Date.parse(range.start)
  const end = Date.parse(range.end)
  if (Number.isNaN(start) || Number.isNaN(end)) return 'Enter both a start and an end.'
  if (end <= start) return 'The end must be after the start.'
  if (end > now + 60_000) return 'The end cannot be in the future.'
  if (end - start < minimumRangeMs) return 'The range must be at least 1 minute.'
  if (end - start > maximumRangeMs) return 'The range can be at most 365 days.'
  return undefined
}

/**
 * Attribution and recommendation windows are bounded to 1h-720h and end now;
 * the closest such window that covers the chart range.
 */
export function analysisWindow(preset: string | undefined, custom?: CustomRange, now = Date.now()): string {
  const span = custom ? now - Date.parse(custom.start) : durationMs(preset ?? '24h')
  const hours = Math.ceil((Number.isNaN(span) ? 24 * 3_600_000 : span) / 3_600_000)
  return `${Math.min(720, Math.max(24, hours))}h`
}

/** Short description of a custom range in the browser time zone. */
export function describeRange(range: CustomRange): string {
  const format = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' })
  return `${format.format(new Date(range.start))} – ${format.format(new Date(range.end))}`
}

export const browserTimeZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || 'local time'
