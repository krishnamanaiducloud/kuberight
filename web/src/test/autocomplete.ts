import { fireEvent, screen } from '@testing-library/react'

/**
 * Picks an option from a MUI Autocomplete the way a user does: open the
 * list, type to filter it, and click the option. The option list renders in
 * a portal, so it is searched in the whole document.
 */
export async function chooseOption(input: HTMLElement, option: string, typed = option) {
  fireEvent.mouseDown(input)
  fireEvent.change(input, { target: { value: typed } })
  fireEvent.click(await screen.findByRole('option', { name: option }, { timeout: 5000 }))
}

/** A namespace inventory page as GET /api/v1/namespaces returns it. */
export const namespacePage = (...names: string[]) => ({
  items: names.map((name) => ({ name })),
  pagination: { page: 1, pageSize: 200, total: names.length },
  status: { available: true },
})
