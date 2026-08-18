<template>
  <div>
    <v-alert type="info" variant="tonal" class="mb-4" density="compact">
      <div class="text-body-2">
        Google Photos Ambient pairs each frame with its own device in your
        Google Photos account, and you pick the albums or people to show from
        the Google Photos app. It needs its own OAuth client of type
        <strong>TVs and Limited Input devices</strong> (the Picker credentials
        above will not work).
      </div>
    </v-alert>

    <v-text-field
      v-model="clientId"
      label="Ambient Client ID"
      variant="outlined"
      density="compact"
      class="mb-2"
    ></v-text-field>

    <v-text-field
      v-model="clientSecret"
      label="Ambient Client Secret"
      type="password"
      variant="outlined"
      density="compact"
      class="mb-3"
    ></v-text-field>

    <v-btn color="grey-darken-1" class="mb-4" @click="saveCredentials"
      >Save Ambient Credentials</v-btn
    >

    <v-divider class="my-6"></v-divider>

    <div class="d-flex align-center justify-space-between mb-3">
      <h3 class="text-subtitle-1 font-weight-bold">Frames</h3>
      <span class="text-caption text-grey">{{ store.count }} photos</span>
    </div>

    <v-alert
      v-if="!devices.length"
      type="warning"
      variant="tonal"
      density="compact"
    >
      Add a device first — each ambient connection belongs to one frame.
    </v-alert>

    <v-card
      v-for="dev in devices"
      :key="dev.id"
      variant="outlined"
      class="mb-3 pa-3"
    >
      <div class="d-flex align-center flex-wrap ga-2">
        <div class="font-weight-medium">{{ dev.name }}</div>
        <v-chip
          v-if="statusOf(dev.id)?.connected"
          size="small"
          color="success"
          variant="tonal"
          >Connected</v-chip
        >
        <v-chip v-else size="small" color="grey" variant="tonal"
          >Not connected</v-chip
        >
        <v-spacer />
        <v-btn
          v-if="!statusOf(dev.id)?.connected && !pairingOf(dev.id)"
          size="small"
          color="primary"
          :disabled="!statusOf(dev.id)?.configured"
          :loading="connecting === dev.id"
          @click="connect(dev)"
        >
          Connect
        </v-btn>
        <v-btn
          v-if="statusOf(dev.id)?.connected || pairingOf(dev.id)"
          size="small"
          color="error"
          variant="text"
          @click="disconnect(dev)"
        >
          Disconnect
        </v-btn>
      </div>

      <!-- Device-code authorization -->
      <div v-if="pairingOf(dev.id)" class="mt-3">
        <div v-if="pairingOf(dev.id)!.status === 'error'">
          <v-alert type="error" variant="tonal" density="compact">
            {{ pairingOf(dev.id)!.error || 'Authorization failed' }}
          </v-alert>
        </div>
        <div
          v-else-if="pairingOf(dev.id)!.user_code"
          class="d-flex flex-wrap align-center ga-4"
        >
          <QrcodeVue
            :value="pairingOf(dev.id)!.verification_url || ''"
            :size="132"
            level="M"
            render-as="svg"
          />
          <div>
            <div class="text-body-2 mb-1">
              Scan the code, or open
              <a
                :href="pairingOf(dev.id)!.verification_url"
                target="_blank"
                rel="noopener"
                >{{ pairingOf(dev.id)!.verification_url }}</a
              >
              and enter:
            </div>
            <div class="text-h5 font-weight-bold">
              {{ pairingOf(dev.id)!.user_code }}
            </div>
            <div class="text-caption text-grey mt-1">
              Waiting for authorization…
            </div>
          </div>
        </div>
        <div v-else class="text-caption text-grey">
          {{ pairingLabel(pairingOf(dev.id)!.status) }}
        </div>
      </div>

      <!-- Connected device details -->
      <div v-if="statusOf(dev.id)?.device?.google_device_id" class="mt-3">
        <div class="text-caption text-grey">
          Google account: {{ statusOf(dev.id)!.device!.account_email || '—' }}
        </div>
        <div class="text-caption text-grey">
          Last sync: {{ formatTime(statusOf(dev.id)!.device!.last_sync_at) }}
        </div>

        <v-alert
          v-if="!statusOf(dev.id)!.device!.media_sources_set"
          type="warning"
          variant="tonal"
          density="compact"
          class="mt-2"
        >
          No photos selected yet for this frame.
        </v-alert>
        <div v-else class="d-flex flex-wrap ga-2 mt-2">
          <v-chip
            v-for="src in statusOf(dev.id)!.media_sources || []"
            :key="src.id"
            size="small"
            variant="tonal"
            color="primary"
          >
            {{ src.displayName || src.id }}
          </v-chip>
        </div>

        <v-alert
          v-if="statusOf(dev.id)!.device!.last_error"
          type="error"
          variant="tonal"
          density="compact"
          class="mt-2"
        >
          {{ statusOf(dev.id)!.device!.last_error }}
        </v-alert>

        <div class="d-flex flex-wrap ga-2 mt-3">
          <v-btn
            v-if="statusOf(dev.id)!.device!.settings_uri"
            size="small"
            color="primary"
            variant="tonal"
            append-icon="mdi-open-in-new"
            :href="statusOf(dev.id)!.device!.settings_uri"
            target="_blank"
            rel="noopener"
          >
            Choose photos in Google Photos
          </v-btn>
          <v-btn size="small" variant="text" @click="rename(dev)">Rename</v-btn>
        </div>
      </div>
    </v-card>

    <div class="d-flex flex-wrap align-center ga-2 mt-4">
      <v-btn
        color="primary"
        variant="tonal"
        :loading="store.loading"
        @click="syncNow"
        >Sync Now</v-btn
      >
      <v-btn color="warning" @click="clearPhotos">Clear All Photos</v-btn>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue';
import QrcodeVue from 'qrcode.vue';
import { useAmbientStore } from '../stores/ambient';
import { useSettingsStore } from '../stores/settings';
import { useSnackbar } from '../composables/useSnackbar';
import { getApiError } from '../utils/errors';

const props = defineProps<{
  devices: { id: number; name: string }[];
}>();

const store = useAmbientStore();
const settingsStore = useSettingsStore();
const { showMessage } = useSnackbar();

const clientId = ref('');
const clientSecret = ref('');
const connecting = ref<number | null>(null);
let poller: number | null = null;

const statusOf = (frameId: number) => store.statuses[frameId];
// Only surface a pairing while it is still in flight; a finished pairing is
// represented by the connected device itself.
const pairingOf = (frameId: number) => {
  const p = store.statuses[frameId]?.pairing;
  return p && p.status !== 'connected' ? p : undefined;
};

const pairingLabel = (status: string) => {
  switch (status) {
    case 'creating_device':
      return 'Creating the ambient device…';
    case 'waiting_for_photos':
      return 'Waiting for you to pick photos in Google Photos…';
    default:
      return 'Waiting for authorization…';
  }
};

const formatTime = (t?: string | null) =>
  t ? new Date(t).toLocaleString() : 'never';

const refreshAll = async () => {
  await Promise.all(
    props.devices.map((d) =>
      store.fetchStatus(d.id).catch(() => {
        // Non-fatal: leave the previous status for this frame.
      })
    )
  );
};

// Poll while any frame is mid-authorization so the card flips to "connected"
// (and shows the photo-selection link) without a manual refresh.
const anyPending = computed(() =>
  props.devices.some((d) => !!pairingOf(d.id) && statusOf(d.id)?.pairing)
);

onMounted(async () => {
  if (!Object.keys(settingsStore.settings).length) {
    await settingsStore.fetchSettings();
  }
  clientId.value = settingsStore.settings.google_ambient_client_id || '';
  clientSecret.value =
    settingsStore.settings.google_ambient_client_secret || '';
  await refreshAll();
  await store.fetchCount();
  poller = window.setInterval(async () => {
    if (!anyPending.value) return;
    await refreshAll();
    await store.fetchCount();
  }, 4000);
});

onUnmounted(() => {
  if (poller !== null) window.clearInterval(poller);
});

const saveCredentials = async () => {
  try {
    await settingsStore.saveSettings({
      google_ambient_client_id: clientId.value,
      google_ambient_client_secret: clientSecret.value,
    });
    await refreshAll();
    showMessage('Ambient credentials saved');
  } catch (e) {
    showMessage(getApiError(e, 'Failed to save credentials'), true);
  }
};

const connect = async (dev: { id: number; name: string }) => {
  connecting.value = dev.id;
  try {
    await store.connect(dev.id, dev.name);
  } catch (e) {
    showMessage(getApiError(e, 'Failed to start authorization'), true);
  } finally {
    connecting.value = null;
  }
};

const disconnect = async (dev: { id: number }) => {
  try {
    await store.disconnect(dev.id);
    showMessage('Ambient device disconnected');
  } catch (e) {
    showMessage(getApiError(e, 'Failed to disconnect'), true);
  }
};

const rename = async (dev: { id: number; name: string }) => {
  const name = window.prompt('Device name shown in Google Photos', dev.name);
  if (!name) return;
  try {
    await store.rename(dev.id, name);
    showMessage('Ambient device renamed');
  } catch (e) {
    showMessage(getApiError(e, 'Failed to rename'), true);
  }
};

const syncNow = async () => {
  try {
    await store.sync();
    await refreshAll();
    await store.fetchCount();
    showMessage('Ambient sync started');
  } catch (e) {
    showMessage(getApiError(e, 'Failed to sync'), true);
  }
};

const clearPhotos = async () => {
  try {
    await store.clear();
    await refreshAll();
    showMessage('Ambient photos cleared');
  } catch (e) {
    showMessage(getApiError(e, 'Failed to clear photos'), true);
  }
};
</script>
