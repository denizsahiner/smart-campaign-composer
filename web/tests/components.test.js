import { describe, it, expect } from 'vitest'
import { mount } from '@vue/test-utils'
import ChannelCounter from '../src/components/ChannelCounter.vue'
import StatusBadge from '../src/components/StatusBadge.vue'
import BaseButton from '../src/components/BaseButton.vue'

describe('ChannelCounter', () => {
  const state = (used, limit) => mount(ChannelCounter, { props: { used, limit } })

  it('shows used / limit', () => {
    expect(state(42, 160).text()).toBe('42 / 160')
  })

  it.each([
    [143, 160, ''], // below 90%
    [144, 160, 'near'], // exactly 90%
    [160, 160, 'near'], // at the limit is still allowed
    [161, 160, 'over'],
    [72, 80, 'near'],
    [81, 80, 'over'],
  ])('%i of %i is "%s"', (used, limit, cls) => {
    const el = state(used, limit).find('.counter')
    expect(el.classes('near')).toBe(cls === 'near')
    expect(el.classes('over')).toBe(cls === 'over')
  })
})

describe('StatusBadge', () => {
  it.each([
    ['draft', 'Taslak', 'muted'],
    ['ready', 'Hazır', 'brand'],
    ['approved', 'Onaylandı', 'success'],
    ['completed', 'Tamamlandı', 'success'],
    ['retry_scheduled', 'Yeniden denenecek', 'warning'],
    ['failed', 'Başarısız', 'danger'],
  ])('%s reads "%s" with the %s color', (status, label, color) => {
    const w = mount(StatusBadge, { props: { status } })
    expect(w.text()).toBe(label)
    expect(w.attributes('style')).toContain(`var(--${color})`)
  })

  it('always pairs the color with a label, even for an unknown status', () => {
    const w = mount(StatusBadge, { props: { status: 'mystery' } })
    expect(w.text()).toBe('mystery')
    expect(w.find('.dot').exists()).toBe(true)
    expect(w.attributes('style')).toContain('var(--muted)')
  })
})

describe('BaseButton', () => {
  it('is disabled while loading and shows an ellipsis', async () => {
    const w = mount(BaseButton, { props: { loading: true }, slots: { default: 'Kaydet' } })
    expect(w.attributes('disabled')).toBeDefined()
    expect(w.text()).toBe('Kaydet…')
  })

  it('defaults to type=button so it never submits a form by accident', () => {
    expect(mount(BaseButton).attributes('type')).toBe('button')
  })
})
