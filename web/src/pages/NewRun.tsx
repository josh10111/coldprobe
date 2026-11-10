import { useEffect, useId, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useActiveRun } from '../ActiveRun'
import { estimate, getDefaults, getGpus } from '../api'
import { Help, Price, StockWord } from '../components'
import { configName, usd } from '../format'
import {
  estimateKey,
  formErrors,
  formFromDefaults,
  GPU_IMAGE,
  needsGpuImage,
  pickerRows,
  rangeHelp,
  stockWarning,
  switchGpuMode,
  toggleGpu,
  toParams,
  type FormState,
  type NumKey,
  type RunBody,
} from '../form'
import { GpuPicker } from '../GpuPicker'
import { errorToast, isLowStock, START_REFUSED_KEY, startFailedToast } from '../toasts'
import { useToasts } from '../Toaster'
import type { ColdStartMode, Estimate, FlashBoot, Limits, Pool } from '../types'

const COLD_MODES: { mode: ColdStartMode; label: string }[] = [
  { mode: 'natural', label: 'Natural' },
  { mode: 'forced', label: 'Forced (new worker)' },
]

export function NewRun() {
  const { start } = useActiveRun()
  const { push, dismiss } = useToasts()
  const refusedToast = useRef<number | null>(null)
  const navigate = useNavigate()

  const [catalog, setCatalog] = useState<Pool[] | null>(null)
  const [capUsd, setCapUsd] = useState(0)
  const [limits, setLimits] = useState<Limits>({})
  const [form, setForm] = useState<FormState | null>(null)
  // Start is enabled only while the estimated body still matches the form,
  // so an edit (or a reply arriving after one) disables it.
  const [est, setEst] = useState<{ key: string; value: Estimate } | null>(null)
  const [problem, setProblem] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  // A low-stock refusal offers "Start anyway" until the form is edited.
  const [lowStock, setLowStock] = useState<{ key: string; body: RunBody; value: Estimate; message: string } | null>(null)

  useEffect(() => {
    let current = true
    Promise.all([getDefaults(), getGpus()])
      .then(([d, pools]) => {
        if (!current) return
        setForm(formFromDefaults(d))
        setCapUsd(d.budgetCapUsd)
        setLimits(d.limits ?? {})
        setCatalog(pools)
      })
      .catch((err) => current && setProblem(err.message))
    return () => {
      current = false
    }
  }, [])

  useEffect(
    () => () => {
      if (refusedToast.current !== null) dismiss(refusedToast.current)
    },
    [dismiss],
  )

  if (!form || !catalog) {
    return (
      <>
        <h1>New run</h1>
        {problem ? <Problem text={`Couldn't load the form: ${problem}`} /> : <p className="mut">Loading…</p>}
      </>
    )
  }

  const params = toParams(form)
  const key = params ? estimateKey(params) : null
  const current = est && est.key === key ? est.value : null
  const errors = formErrors(form, limits, capUsd)
  const invalid = Object.keys(errors).length > 0
  const refusal = lowStock && lowStock.key === key ? lowStock : null
  const helpFor = (k: NumKey, text: string) => [text, rangeHelp(k, limits)].filter(Boolean).join(' ')
  const levelCount = limits.levelsRps
  const levelRps = limits.rps

  // Any edit drops a low-stock refusal, even one that is later undone.
  const change = (next: FormState) => {
    setForm(next)
    setLowStock(null)
  }
  const edit = (patch: Partial<FormState>) => change({ ...form, ...patch })
  const setNum = (k: NumKey, v: string) => edit({ nums: { ...form.nums, [k]: v } })

  const rows = pickerRows(catalog, form.gpuMode)
  const selected = form.gpuMode === 'card' ? form.cards : form.pools
  const toggleRow = (id: string) => {
    const next = toggleGpu(selected, id, rows.map((r) => r.id))
    edit(form.gpuMode === 'card' ? { cards: next } : { pools: next })
  }
  const toggleFlash = (f: FlashBoot) => {
    const on = new Set(form.flashboot)
    if (on.has(f)) on.delete(f)
    else on.add(f)
    edit({ flashboot: (['OFF', 'FLASHBOOT'] as const).filter((x) => on.has(x)) })
  }

  async function onEstimate() {
    if (!params || !key || invalid) return
    setBusy(true)
    setProblem(null)
    try {
      setEst({ key, value: await estimate(params) })
    } catch (err) {
      setProblem((err as Error).message)
      push(errorToast('Estimate failed', err))
    } finally {
      setBusy(false)
    }
  }

  async function begin(body: RunBody, value: Estimate, allowLowStock: boolean) {
    if (refusedToast.current !== null) dismiss(refusedToast.current)
    refusedToast.current = null
    setLowStock(null)
    setBusy(true)
    setProblem(null)
    try {
      const id = await start(body, value, { allowLowStock })
      navigate(`/runs/${id}`)
    } catch (err) {
      const spec = startFailedToast(err)
      const id = push(spec)
      if (spec.key === START_REFUSED_KEY) refusedToast.current = id
      if (isLowStock(err)) setLowStock({ key: estimateKey(body), body, value, message: err.message })
      else setProblem((err as Error).message)
      setBusy(false)
    }
  }

  function onStart() {
    if (params && current && !invalid) void begin(params, current, false)
  }

  const warnings = current ? stockWarning(current.configs) : []

  return (
    <div className="form">
      <h1>New run</h1>
      <p className="mut">coldprobe measures Runpod Serverless endpoints; Pods are out of scope.</p>

      <h2 className="section">GPUs to benchmark</h2>
      <p className="section-sub">
        Up to two. Pick by pool (Runpod decides the card) or by card (consistent silicon, better for matmul numbers).
      </p>
      <GpuPicker
        mode={form.gpuMode}
        rows={rows}
        selected={selected}
        onMode={(m) => change(switchGpuMode(form, m))}
        onToggle={toggleRow}
      />
      {warnings.map((w) => (
        <p key={w.level} className={w.level === 'NONE' ? 'bad small' : 'warn small'}>
          {w.text}
        </p>
      ))}

      <div className="lbl">
        FlashBoot
        <Help text="Runpod's faster cold start; pick both to compare it on and off." />
      </div>
      <div className="toggles">
        {(['OFF', 'FLASHBOOT'] as const).map((f) => {
          const on = form.flashboot.includes(f)
          return (
            <button key={f} type="button" className={on ? 'toggle on' : 'toggle'} aria-pressed={on} onClick={() => toggleFlash(f)}>
              {f === 'OFF' ? 'Off' : 'On'}
            </button>
          )
        })}
      </div>

      <div className="grid3">
        <NumField
          label="Max workers"
          help={helpFor(
            'maxWorkers',
            "The most workers each endpoint may run at once. Runpod's default is 3; your account's balance sets how many it will actually grant. More workers serve higher load levels but cost more: the estimate bills maxWorkers + 1 per endpoint.",
          )}
          k="maxWorkers"
          form={form}
          limits={limits}
          error={errors.maxWorkers}
          onChange={setNum}
        />
        <NumField label="Cold starts" help={helpFor('coldStarts', 'How many cold-start samples to take per config, each after workers scale to zero.')} k="coldStarts" form={form} limits={limits} error={errors.coldStarts} onChange={setNum} />
        <NumField label="Warm requests" help={helpFor('warmRequests', 'How many requests to send one after another to a running worker.')} k="warmRequests" form={form} limits={limits} error={errors.warmRequests} onChange={setNum} />
      </div>

      <div className="lbl">
        Cold starts: how to get each one
        <Help text="Natural waits for Runpod to scale the workers to zero on its own (FlashBoot may keep one warm). Forced pauses the endpoint before each sample so the request must start a brand-new worker. FlashBoot can't help there, so expect ~70–80 s either way, and ~75 s more run time per sample." />
      </div>
      <div className="toggles" role="radiogroup" aria-label="Cold start mode">
        {COLD_MODES.map(({ mode, label }) => (
          <button
            key={mode}
            type="button"
            role="radio"
            aria-checked={form.coldStartMode === mode}
            className={form.coldStartMode === mode ? 'toggle on' : 'toggle'}
            onClick={() => edit({ coldStartMode: mode })}
          >
            {label}
          </button>
        ))}
      </div>

      <div className="grid3">
        <TextField
          label="Load levels (req/s)"
          help={
            'Request rates to hold in turn, comma-separated' +
            (levelCount && levelRps ? `: up to ${levelCount[1]}, each above ${levelRps[0]} and at most ${levelRps[1]}.` : '.')
          }
          value={form.levels}
          error={errors.levels}
          onChange={(v) => edit({ levels: v })}
        />
        <NumField label="Seconds per level" help={helpFor('levelSeconds', 'How long each load level sends requests.')} k="levelSeconds" form={form} limits={limits} error={errors.levelSeconds} onChange={setNum} />
        <NumField label="Job length (ms)" help={helpFor('jobSleepMs', 'How long the worker sleeps per request, standing in for real work.')} k="jobSleepMs" form={form} limits={limits} error={errors.jobSleepMs} onChange={setNum} />
      </div>

      <div className="matmul">
        <NumField
          label="Matmul size (0 = off)"
          help={helpFor(
            'matmulN',
            'Side length of the fp16 square matmul each job runs on the GPU. 0 skips it. 4096 is a good size: large enough to keep the whole GPU busy so the TFLOPS figure means something (1024 and below read low), small enough to fit any card, and what the recorded GPU run used.',
          )}
          k="matmulN"
          form={form}
          limits={limits}
          error={errors.matmulN}
          onChange={setNum}
        />
        <p className="mut small">
          Measures fp16 throughput and $ per TFLOP-hour per card. Needs the GPU image (larger, slower first boot).
        </p>
      </div>
      {needsGpuImage(Number(form.nums.matmulN), form.image) && (
        <p className="warn small">
          Matmul needs the GPU image.{' '}
          <button type="button" className="btn" onClick={() => edit({ image: GPU_IMAGE })}>
            Use GPU image
          </button>
        </p>
      )}

      <details className="advanced">
        <summary>Advanced: image, idle timeout, model load, drain</summary>
        <TextField label="Worker image" help="The container image every endpoint runs." value={form.image} onChange={(v) => edit({ image: v })} />
        <div className="grid3">
          <NumField label="Idle timeout (s)" help={helpFor('idleTimeoutS', 'How long a worker stays up with no requests before scaling to zero.')} k="idleTimeoutS" form={form} limits={limits} error={errors.idleTimeoutS} onChange={setNum} />
          <NumField label="Model load (s)" help={helpFor('modelLoadS', 'How long the worker pretends to load a model when it boots.')} k="modelLoadS" form={form} limits={limits} error={errors.modelLoadS} onChange={setNum} />
          <NumField label="Drain (s)" help={helpFor('drainSeconds', "How long to wait for a load level's last requests to finish.")} k="drainSeconds" form={form} limits={limits} error={errors.drainSeconds} onChange={setNum} />
        </div>
      </details>

      <div className="budget">
        <NumField label="Budget ($)" help="The most this run may spend; the budget guard stops it before its spend bound passes this." k="budgetUsd" form={form} limits={limits} error={errors.budgetUsd} onChange={setNum} step="0.01" max={capUsd} />
        <span className="mut small">
          server cap {usd(capUsd)}
          <Help text="The largest budget this server allows for one run (COLDPROBE_BUDGET_CAP_USD)." />
        </span>
      </div>

      {current && <EstimateBox est={current} />}
      {problem && <Problem text={problem} />}

      {refusal && (
        <p className="warn small refusal" role="status">
          {refusal.message}
        </p>
      )}
      <div className="actions">
        <button type="button" className="btn" disabled={!params || invalid || busy} onClick={onEstimate}>
          Estimate
        </button>{' '}
        <button type="button" className="btn primary" disabled={!current || invalid || busy} onClick={onStart}>
          Start run
        </button>
        {refusal && (
          <button type="button" className="btn" disabled={busy} onClick={() => void begin(refusal.body, refusal.value, true)}>
            Start anyway
          </button>
        )}
      </div>
      <p className="mut small">
        {invalid
          ? 'Fix the fields marked in red to estimate.'
          : 'Editing any field clears the estimate and disables Start again.'}
      </p>
    </div>
  )
}

function EstimateBox({ est }: { est: Estimate }) {
  const capped = est.worstCaseUsd > est.capUsd
  const n = est.configs.length
  return (
    <section className="estimate">
      <h2>
        Estimate{' '}
        <span className="mut small">
          {n} config{n === 1 ? '' : 's'}
          <Help text="Each pool or card is measured once per FlashBoot setting, each on its own temporary endpoint." />
        </span>
      </h2>
      {capped ? (
        <p>
          Worst case <span className="price">{usd(est.worstCaseUsd)}</span>, capped at{' '}
          <span className="price">{usd(est.capUsd)}</span>
          <Help text="Worst case bills every endpoint for max workers + 1 from creation until every phase times out." />
          <br />
          <span className="mut">
            The guard stops the run if the spend bound reaches {usd(est.capUsd)} (after ~
            {Math.ceil(est.guardStopsAfterS / 60)} min at most).
          </span>
        </p>
      ) : (
        <p>
          May spend up to <span className="price">{usd(est.maxSpendUsd)}</span>
          <Help text="Worst case bills every endpoint for max workers + 1 from creation until every phase times out." />
          <br />
          <span className="mut">That worst case is within the {usd(est.capUsd)} budget.</span>
        </p>
      )}
      <div className="table-wrap">
        <table className="spec">
          <thead>
            <tr>
              <th>Configuration</th>
              <th className="n">
                Price
                <Help text="The price per GPU-hour this config is costed at: the card's own, or the pool's dearest card." />
              </th>
              <th className="n">
                Stock now
                <Help text="Runpod's stock level for this pool or card when the estimate was made; Start checks it again." />
              </th>
              <th className="n">
                Worst case
                <Help text="This config's own worst case; together they add up to the total worst case." />
              </th>
            </tr>
          </thead>
          <tbody>
            {est.configs.map((c) => (
              <tr key={`${c.card ?? c.pool}-${c.flashboot}`}>
                <td>{configName(c)}</td>
                <td className="n">
                  <Price usd={c.priceHr} unit="/hr" />
                </td>
                <td className="n">
                  <StockWord level={c.availability} />
                </td>
                <td className="n">{usd(c.worstCaseUsd)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  )
}

function Problem({ text }: { text: string }) {
  return (
    <div className="problem" role="alert">
      {text}
    </div>
  )
}

interface NumFieldProps {
  label: string
  help: string
  k: NumKey
  form: FormState
  limits: Limits
  error?: string
  onChange: (k: NumKey, v: string) => void
  step?: string
  max?: number
}

// min and max only steer the spinner: an out-of-range value is marked invalid, never clamped.
function NumField({ label, help, k, form, limits, error, onChange, step, max }: NumFieldProps) {
  const id = useId()
  const errId = useId()
  const range = limits[k]
  return (
    <div>
      <div className="lbl">
        <label htmlFor={id}>{label}</label>
        <Help text={help} />
      </div>
      <input
        id={id}
        className={error ? 'in invalid' : 'in'}
        type="number"
        inputMode="decimal"
        step={step}
        min={range ? range[0] : 0}
        max={max ?? range?.[1]}
        value={form.nums[k]}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? errId : undefined}
        onChange={(e) => onChange(k, e.target.value)}
      />
      {error && (
        <div id={errId} className="field-error">
          {error}
        </div>
      )}
    </div>
  )
}

function TextField(props: { label: string; help: string; value: string; error?: string; onChange: (v: string) => void }) {
  const id = useId()
  const errId = useId()
  return (
    <div>
      <div className="lbl">
        <label htmlFor={id}>{props.label}</label>
        <Help text={props.help} />
      </div>
      <input
        id={id}
        className={props.error ? 'in invalid' : 'in'}
        value={props.value}
        aria-invalid={props.error ? true : undefined}
        aria-describedby={props.error ? errId : undefined}
        onChange={(e) => props.onChange(e.target.value)}
      />
      {props.error && (
        <div id={errId} className="field-error">
          {props.error}
        </div>
      )}
    </div>
  )
}
