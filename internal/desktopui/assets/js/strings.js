// Every user-facing state word lives here so it can be reviewed and translated.
export const STATES = {
  stopped: { word: 'Stopped', tag: null, label: 'offline' },
  starting: { word: 'Starting', tag: 'warning', icon: 'clock', label: 'working' },
  ready: { word: 'Ready', tag: 'success', icon: 'check', label: 'online' },
  updating: { word: 'Updating', tag: 'warning', icon: 'clock', label: 'working' },
  setup: { word: 'Setup needed', tag: 'warning', icon: 'alert', label: 'action needed' },
  restart: { word: 'Restart needed', tag: 'warning', icon: 'alert', label: 'action needed' },
  error: { word: 'Error', tag: 'danger', icon: 'alert', label: 'failed' },
};

export function stateKey(s) {
  if (s.restartRequired || s.phase === 'restart required') return 'restart';
  if (s.setupRequired || s.phase === 'setup needed') return 'setup';
  switch (s.phase) {
    case 'ready': return 'ready';
    case 'error': return 'error';
    case 'stopped': return 'stopped';
    case 'updating': case 'preparing update': case 'setup': return 'updating';
    default: return 'starting';
  }
}

export const HOST_ASK = {
  resources: r => 'Resize the appliance to ' + r.resources.cpus + ' CPUs and ' + r.resources.memoryMiB + ' MiB of memory' + (r.resources.diskGiB ? ' and grow its data disk to ' + r.resources.diskGiB + ' GiB (never shrinks)' : '') + ' (takes effect at the next restart)',
  power: r => 'Appliance: ' + r.power.action,
  'recovery-point': r => 'Stop the appliance for a few minutes, write an encrypted recovery point of everything to destination ' + r.recoveryPoint.destination + ', check it, and start again',
  'backup-schedule': r => r.backupSchedule.enabled
    ? 'Every ' + r.backupSchedule.everyHours + ' hours, between ' + r.backupSchedule.windowStartHour + ':00 and ' + ((r.backupSchedule.windowStartHour + r.backupSchedule.windowHours) % 24) + ':00, stop the appliance for a few minutes and write a recovery point to ' + r.backupSchedule.destination + ', keeping the newest ' + r.backupSchedule.keepLast
    : 'Cancel the scheduled recovery points',
  autostart: r => (r.autostart.enabled ? 'Start' : 'Do not start') + ' Containers Anywhere automatically at login',
  'prune-images': () => 'Remove unused container images (volumes and images in use are never touched)',
  'lan-check': () => "Check this computer's network addresses",
};

export const WARN = {
  prepare: 'Stop the cluster and prepare a local cold recovery point? This is not off-device protection.',
  activate: 'Candidate workloads can execute cron, webhooks and external writes immediately. There is no isolation or automatic rollback. Activate or retry with the CURRENT disk?',
  commit: 'Have you verified your workload data/PVC markers and credentials through this candidate? K3s/Kite health alone is insufficient.',
  cancel: 'Cancel preparation without deleting retained evidence?',
  swap: 'Your cluster restarts and may be unavailable for a few minutes. Your data is not changed, and it goes back to the current version by itself if the update does not work.',
};
