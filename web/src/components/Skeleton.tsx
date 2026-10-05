// Skeleton holds the place of what is loading, so the page is never blank
// before the first answer (spec-ui 7.1).
export function Skeleton({ lines = 3, label = 'Loading' }: { lines?: number; label?: string }) {
  return (
    <div className="skeleton" role="status">
      <span className="sr-only">{label}</span>
      {Array.from({ length: lines }, (_, at) => (
        <span key={at} className="skeleton-line" aria-hidden="true" />
      ))}
    </div>
  )
}
