import { createContext, type ReactNode, useCallback, useContext, useEffect, useRef, useState } from 'react'

import { IconButton } from './Button'
import { CloseIcon, ToneIcon } from './icons'

type ToastTone = 'ok' | 'info' | 'fail'

interface Item {
  id: number
  tone: ToastTone
  text: ReactNode
}

type Show = (text: ReactNode, tone?: ToastTone) => void

const ToastContext = createContext<Show | null>(null)

// How long a result stays; a failure stays until it is dismissed.
const shownFor = 6000

// ToastProvider shows the results of actions in the corner of the page. They
// are announced as they come: failures at once, the others politely
// (spec-ui 10).
export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Item[]>([])
  const next = useRef(0)
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>())

  const dismiss = useCallback((id: number) => setItems((all) => all.filter((t) => t.id !== id)), [])

  const show = useCallback<Show>(
    (text, tone = 'info') => {
      const id = ++next.current
      setItems((all) => [...all, { id, tone, text }])
      if (tone === 'fail') return
      const timer = setTimeout(() => {
        timers.current.delete(timer)
        dismiss(id)
      }, shownFor)
      timers.current.add(timer)
    },
    [dismiss],
  )

  useEffect(() => {
    const pending = timers.current
    return () => {
      for (const timer of pending) clearTimeout(timer)
    }
  }, [])

  const list = (failures: boolean) =>
    items
      .filter((t) => (t.tone === 'fail') === failures)
      .map((t) => (
        <div key={t.id} className={`toast toast-${t.tone}`}>
          <ToneIcon tone={t.tone} />
          <div className="toast-text">{t.text}</div>
          <IconButton label="Dismiss" icon={<CloseIcon />} onClick={() => dismiss(t.id)} />
        </div>
      ))

  return (
    <ToastContext value={show}>
      {children}
      <div className="toasts">
        <div aria-live="assertive">{list(true)}</div>
        <div aria-live="polite">{list(false)}</div>
      </div>
    </ToastContext>
  )
}

// useToast returns the function that shows a toast.
export function useToast(): Show {
  const show = useContext(ToastContext)
  if (!show) throw new Error('useToast is used outside of a ToastProvider')
  return show
}
