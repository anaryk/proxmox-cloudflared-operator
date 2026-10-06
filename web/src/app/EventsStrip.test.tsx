import { render } from '@testing-library/react'
import { expect, test } from 'vitest'

import { StoreProvider } from '../api/store'
import populated from '../fixtures/populated.json'
import { fakeStore } from '../test/store'
import { EventsStrip } from './EventsStrip'

test('the strip says its height to the page, and takes it back when it goes', async () => {
  const { store } = await fakeStore({ state: populated })
  const { unmount } = render(
    <StoreProvider store={store}>
      <EventsStrip />
    </StoreProvider>,
  )
  expect(document.documentElement.style.getPropertyValue('--strip-height')).toMatch(/^\d+px$/)
  unmount()
  expect(document.documentElement.style.getPropertyValue('--strip-height')).toBe('')
})
