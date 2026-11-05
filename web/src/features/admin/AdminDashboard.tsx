import { useState } from 'react'
import { MembersPanel } from './MembersPanel'
import { RequestsPanel } from './RequestsPanel'

export function AdminDashboard() {
  const [section, setSection] = useState<'requests' | 'members'>('requests')
  return (
    <main>
      <div className="mb-6 flex gap-2">
        {(['requests', 'members'] as const).map((item) => (
          <button key={item} className={section === item ? 'button-primary' : 'button-secondary'} onClick={() => setSection(item)}>
            {item === 'requests' ? 'Access requests' : 'Members'}
          </button>
        ))}
      </div>
      {section === 'requests' ? <RequestsPanel /> : <MembersPanel />}
    </main>
  )
}
