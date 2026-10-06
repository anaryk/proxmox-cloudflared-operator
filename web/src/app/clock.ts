import { useEffect, useState } from 'react'

// useNow is the time, again every interval: for what says how long ago.
export function useNow(every = 1000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), every)
    return () => clearInterval(timer)
  }, [every])
  return now
}
