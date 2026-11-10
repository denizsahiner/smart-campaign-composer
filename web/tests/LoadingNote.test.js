import { describe, it, expect, afterEach, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import LoadingNote from '../src/components/LoadingNote.vue'

describe('LoadingNote', () => {
  afterEach(() => vi.useRealTimers())

  it('stays hidden for a fast load and appears for a slow one', async () => {
    vi.useFakeTimers()
    const w = mount(LoadingNote, { attrs: { class: 'section-gap' } })
    vi.advanceTimersByTime(299)
    await nextTick()
    expect(w.text()).toBe('')
    vi.advanceTimersByTime(1)
    await nextTick()
    expect(w.text()).toBe('Yükleniyor…')
    expect(w.classes()).toEqual(['muted', 'section-gap'])
  })
})
