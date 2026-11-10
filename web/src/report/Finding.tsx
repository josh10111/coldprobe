import { Help } from '../components'
import type { Finding as FindingText } from '../finding'

export function Finding({ finding }: { finding: FindingText | null }) {
  if (!finding) return null
  return (
    <div className="finding">
      {finding.sentences.map((s, i) => (
        <p key={i}>
          {s}
          {i === finding.sentences.length - 1 && <Help text={finding.help} />}
        </p>
      ))}
    </div>
  )
}
