export const SECTIONS = [
  { id: 'cluster', label: 'Cluster' },
  { id: 'workloads', label: 'Workloads' },
  { id: 'recovery', label: 'Recovery' },
  { id: 'store', label: 'Store' },
  { id: 'updates', label: 'Updates' },
];
export const ROUTES = [...SECTIONS.map(s => s.id), 'diagnostics'];
