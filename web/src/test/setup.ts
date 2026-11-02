import { cleanup, configure } from '@testing-library/react'
import { afterEach } from 'vitest'

// Testing Library unmounts by itself only where the test functions are
// globals, which they are not here.
afterEach(cleanup)

// findBy and waitFor give up after 1 s by default, which a busy runner does not always keep to.
configure({ asyncUtilTimeout: 5000 })
