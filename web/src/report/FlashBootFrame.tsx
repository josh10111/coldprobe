import { Help } from '../components'
import type { FlashBootReadout } from '../finding'
import { secs } from '../format'
import { Frame } from './common'

export function FlashBootFrame({ readouts, forced }: { readouts: FlashBootReadout[]; forced: boolean }) {
  return (
    <Frame
      title="FlashBoot, in this run"
      help="FlashBoot is Runpod's option to keep a stopped worker ready for a faster restart; this frame sets FlashBoot off and on side by side for each pool that ran both."
      sub={
        forced
          ? 'Forced cold starts always land on a new worker; FlashBoot cannot help one.'
          : 'Shown on its own: the finding above compares pools with FlashBoot off.'
      }
    >
      {readouts.length === 0 && <p className="mut small">No pool ran with FlashBoot both off and on, so there is nothing to set side by side.</p>}
      <div className="readouts">
        {readouts.map((r) =>
          r.kind === 'cold' ? (
            <div key={`cold-${r.subject}`} className="readout">
              <h3>
                {forced ? 'New-worker cold starts' : 'Natural cold starts'}, {r.subject}
                <Help
                  text={
                    forced
                      ? "Typical new-worker cold start with FlashBoot off and on; never coloured, because FlashBoot can't help a brand-new worker."
                      : 'Typical cold start with FlashBoot off and on; the lower is green only when it is at least 5% lower with at least 3 samples a side.'
                  }
                />
              </h3>
              <div className="read">
                <span className={r.lower === 0 ? 'ok' : undefined}>{secs(r.off)}</span> <span className="mut">off</span> /{' '}
                <span className={r.lower === 1 ? 'ok' : undefined}>{secs(r.on)}</span> <span className="mut">on</span>
              </div>
              <div className="cap">{r.note}</div>
            </div>
          ) : (
            <div key={`kept-${r.subject}`} className="readout">
              <h3>
                Idle worker, {r.subject}
                <Help text={r.note.help} />
              </h3>
              <div className="read">kept alive</div>
              <div className="cap">
                {r.note.text}; idle time not billed
              </div>
            </div>
          ),
        )}
      </div>
    </Frame>
  )
}
