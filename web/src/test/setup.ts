import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// Testing Library unmounts by itself only where the test functions are
// globals, which they are not here.
afterEach(cleanup)
