import { h, clear, jsonKey } from './dom.js';
import { call, getJSON } from './api.js';
import { confirmSheet } from './sheet.js';
import { callout, guard, toast, card, wide } from './ui.js';
import { receipt } from './receipt.js';

const when = t => (t ? new Date(t).toLocaleString() : 'unknown');
const gib = n => (Math.round((n / 1073741824) * 10) / 10) + ' GiB';

function pointReceipt(x, reload) {
  const scope = x.label === 'system' ? 'cluster system state' : 'the cluster and PVC data';
  const actions = h('div', { class: 'ca-actions' });
  if (x.restorable) {
    actions.append(h('button', {
      class: 'ls-btn ls-btn--danger', type: 'button',
      onclick: async e => {
        const ok = await confirmSheet({
          title: 'Restore this point?',
          body: [
            h('p', null, 'Restore ' + scope + ' to ' + when(x.createdAt) + '.'),
            h('p', null, 'Later changes in that scope are replaced. A safety point is kept first. The appliance restarts now.'),
          ],
          confirm: 'Restore', danger: true,
        });
        if (!ok) return;
        await guard(e.target, async () => { await call('restore-points/restore', 'POST', { id: x.id }); toast('Restore started'); });
        reload();
      },
    }, 'Restore'));
  } else {
    actions.append(h('span', { class: 'ca-meta' }, x.reason || 'Not restorable'));
  }
  return h('li', null, receipt(x.label === 'system' ? 'System point' : 'Full point', [
    ['Taken', when(x.createdAt)],
    ['Appliance', String(x.from || '')],
    ['Volumes', String((x.volumes || []).length)],
  ], actions));
}

export function mount(root, app, sc) {
  const pointsBox = h('ul', { class: 'ca-receipts' });
  const pointNote = h('div');
  const destBox = h('ul', { class: 'ca-list' });
  const backupBox = h('div');
  const keyBox = h('pre', { class: 'ca-key', hidden: true });
  let pk = '', dk = '', bk = '';

  const loadPoints = async () => {
    let d;
    try { d = await getJSON('restore-points'); } catch (e) { return; }
    const k = jsonKey(d);
    if (k === pk) return;
    pk = k;
    clear(pointNote);
    if (d.pending) pointNote.append(callout('info', h('p', null, 'A recovery operation is waiting for the next start: ' + d.pending)));
    if (d.last && d.last.id) pointNote.append(callout(d.last.ok ? 'success' : 'error', h('p', null, 'Last restore ' + d.last.id + (d.last.ok ? ' completed.' : ' did not complete: ' + (d.last.error || '')))));
    clear(pointsBox);
    const list = d.points || [];
    if (!list.length) pointsBox.append(h('li', null, h('div', { class: 'ls-slot' }, h('b', null, 'No recovery points yet'), 'Take one before you change something important.')));
    for (const x of list) pointsBox.append(pointReceipt(x, () => { pk = ''; loadPoints(); }));
  };

  const loadDestinations = async () => {
    let d;
    try { d = await getJSON('destinations'); } catch (e) { return; }
    const k = jsonKey(d);
    if (k === dk) return;
    dk = k;
    clear(destBox);
    const list = d.destinations || [];
    if (!list.length) destBox.append(h('li', { class: 'ca-row' }, 'No destinations yet.'));
    for (const x of list) {
      destBox.append(h('li', { class: 'ca-row' },
        h('p', null, h('b', null, x.name), ' ', h('span', { class: 'ca-mono ca-meta' }, x.path)),
        h('p', { class: 'ca-meta' }, x.available ? gib(x.freeBytes) + ' free' : 'Unavailable: ' + x.lastError),
        x.sameDeviceAsData ? callout('warning', h('p', null, 'Same disk as your data: this is not disaster recovery.')) : null,
        h('div', { class: 'ca-actions' }, h('button', {
          class: 'ls-btn ls-btn--danger', type: 'button',
          onclick: async e => {
            const ok = await confirmSheet({ title: 'Stop using this destination?', body: h('p', null, 'Files already in the folder are not deleted.'), confirm: 'Remove', danger: true });
            if (!ok) return;
            await guard(e.target, () => call('destinations/remove', 'POST', { id: x.id }));
            dk = ''; loadDestinations();
          },
        }, 'Remove'))));
    }
  };

  const loadBackups = async () => {
    let d;
    try { d = await getJSON('recovery'); } catch (e) { return; }
    const k = jsonKey(d);
    if (k === bk) return;
    bk = k;
    clear(backupBox);
    const s = d.schedule;
    if (s && s.enabled) {
      backupBox.append(s.objectiveBreached
        ? callout('warning', h('p', null, 'Behind schedule. ' + s.note))
        : callout('info', h('p', null, 'Every ' + s.everyHours + ' h to ' + s.destination + ', keeping ' + s.keepLast + '. Next due ' + s.nextDue + '.')));
    }
    if (d.kit && d.kit.note) backupBox.append(callout('info', h('p', null, d.kit.note)));
    const list = (d.points || []).slice().reverse().slice(0, 8);
    if (!list.length && !(s && s.enabled)) backupBox.append(h('p', null, 'No encrypted backups yet.'));
    for (const x of list) {
      backupBox.append(receipt(x.phase, [
        ['Started', when(x.startedAt)],
        ['Destination', String(x.destination)],
        ['Checked', x.verified ? 'yes' : 'no'],
      ].concat(x.error ? [['Error', x.error]] : [])));
    }
  };

  root.append(h('div', { class: 'ca-grid' },
    wide(card('Recovery points',
    callout('info', h('p', null, 'System points capture cluster state; full points also capture volume contents. These local points share the data disk, and they are not point-in-time recovery of any application.')),
    pointNote, pointsBox,
    h('div', { class: 'ca-actions' }, h('button', {
      class: 'ls-btn ls-btn--primary', type: 'button',
      onclick: async e => {
        const ok = await confirmSheet({
          title: 'Take a full recovery point?',
          body: h('p', null, 'This briefly stops your workloads, captures the persistent state, then starts Kubernetes again.'),
          confirm: 'Restart and take point',
        });
        if (!ok) return;
        await guard(e.target, async () => { await call('restore-points/take', 'POST', {}); toast('Recovery point taken'); });
        pk = ''; loadPoints();
      },
    }, 'Take a recovery point')))),

    card('Backup destinations',
    h('p', { class: 'ca-meta' }, 'Folders or disks backups can be written to. A destination on the same disk as your data is not disaster recovery.'),
    destBox,
    h('div', { class: 'ca-inline' },
      h('div', { class: 'ls-field' }, h('label', { for: 'dest-name' }, 'Name'), h('input', { class: 'ls-input', id: 'dest-name', maxlength: '80', placeholder: 'Backup disk' })),
      h('div', { class: 'ls-field' }, h('label', { for: 'dest-path' }, 'Folder'), h('input', { class: 'ls-input', id: 'dest-path', placeholder: '/Volumes/Backup' })),
      h('button', {
        class: 'ls-btn', type: 'button',
        onclick: async e => {
          const name = document.getElementById('dest-name'), path = document.getElementById('dest-path');
          await guard(e.target, async () => {
            await call('destinations/add', 'POST', { name: name.value, path: path.value });
            name.value = ''; path.value = '';
          });
          dk = ''; loadDestinations();
        },
      }, 'Add destination'))),

    card('Encrypted backups',
    h('p', { class: 'ca-meta' }, 'Whole-appliance copies, encrypted before they reach a destination. Each one stops the appliance for a few minutes.'),
    backupBox),

    card('Recovery key',
    h('p', { class: 'ca-meta' }, 'You need this key to restore an encrypted backup. Keep a copy somewhere other than this computer.'),
    keyBox,
    h('div', { class: 'ca-actions' },
      h('button', {
        class: 'ls-btn', type: 'button',
        onclick: e => guard(e.target, async () => { keyBox.textContent = await call('recovery/key', 'POST', {}); keyBox.hidden = false; }),
      }, 'Show recovery key'),
      h('button', {
        class: 'ls-btn', type: 'button',
        onclick: e => guard(e.target, async () => {
          await call('recovery/key-saved', 'POST', {});
          keyBox.hidden = true; keyBox.textContent = '';
          toast('Noted. The key is hidden again.');
          bk = ''; loadBackups();
        }),
      }, 'I saved the key somewhere else')))));

  sc.every(15000, loadPoints);
  sc.every(15000, loadDestinations);
  sc.every(10000, loadBackups);
}
