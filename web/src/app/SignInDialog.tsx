import { useApp, useStore } from '../api/store'
import { Dialog } from '../components/Dialog'
import { SignInCard } from '../pages/SignIn'

// SignInDialog opens over the page when a call finds the session gone: the
// page keeps what it shows, and what the user did last is not sent again
// after the sign-in (spec-ui 7.3).
export function SignInDialog() {
  const store = useStore()
  const needed = useApp((s) => s.signInNeeded)
  return (
    <Dialog open={needed} onClose={() => store.dismissSignIn()} title="Your session has ended">
      <p>Sign in again to go on. The page keeps what it shows; what you did last was not sent again.</p>
      <SignInCard />
    </Dialog>
  )
}
