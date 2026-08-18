import { defineStore } from 'pinia';
import { api } from '../api';
import { getApiError } from '../utils/errors';

// Google Photos Ambient pairing state. Unlike the other photo sources, ambient
// state is per frame: each frame is its own device in the user's Google Photos
// account, with its own authorization and photo selection.

export interface AmbientPairing {
  status:
    | 'pending_authorization'
    | 'creating_device'
    | 'waiting_for_photos'
    | 'connected'
    | 'error';
  user_code?: string;
  verification_url?: string;
  expires_at?: string;
  error?: string;
}

export interface AmbientDevice {
  id: number;
  device_id: number;
  google_device_id: string;
  display_name: string;
  settings_uri: string;
  media_sources_set: boolean;
  poll_interval_seconds: number;
  account_email: string;
  last_sync_at?: string | null;
  last_error: string;
}

export interface AmbientStatus {
  configured: boolean;
  connected: boolean;
  device?: AmbientDevice;
  media_sources?: { id: string; displayName?: string }[];
  pairing?: AmbientPairing;
  syncing: boolean;
  photo_count: number;
}

export const useAmbientStore = defineStore('ambient', {
  state: () => ({
    // Keyed by local frame (device) id.
    statuses: {} as Record<number, AmbientStatus>,
    count: 0,
    loading: false,
    error: null as string | null,
  }),
  actions: {
    async fetchStatus(frameId: number) {
      const res = await api.get(`/devices/${frameId}/ambient`);
      this.statuses = { ...this.statuses, [frameId]: res.data };
      return res.data as AmbientStatus;
    },

    // Starts the device-code flow; the returned code/URL is what the user
    // enters (or scans) to authorize the frame.
    async connect(frameId: number, displayName: string) {
      this.loading = true;
      this.error = null;
      try {
        const res = await api.post(`/devices/${frameId}/ambient/connect`, {
          display_name: displayName,
        });
        await this.fetchStatus(frameId);
        return res.data as AmbientPairing;
      } catch (e) {
        this.error = getApiError(e);
        throw e;
      } finally {
        this.loading = false;
      }
    },

    async rename(frameId: number, displayName: string) {
      await api.put(`/devices/${frameId}/ambient`, {
        display_name: displayName,
      });
      await this.fetchStatus(frameId);
    },

    async disconnect(frameId: number) {
      this.loading = true;
      try {
        await api.delete(`/devices/${frameId}/ambient`);
        await this.fetchStatus(frameId);
        await this.fetchCount();
      } finally {
        this.loading = false;
      }
    },

    async fetchCount() {
      try {
        const res = await api.get('/google-ambient/count');
        this.count = res.data.count || 0;
      } catch (e) {
        console.error('Failed to fetch ambient photo count', e);
      }
    },

    async sync() {
      this.loading = true;
      try {
        await api.post('/google-ambient/sync');
      } finally {
        this.loading = false;
      }
    },

    async syncStatus() {
      const res = await api.get('/google-ambient/sync-status');
      return res.data as { running: boolean };
    },

    async clear() {
      this.loading = true;
      try {
        await api.post('/google-ambient/clear');
        await this.fetchCount();
      } finally {
        this.loading = false;
      }
    },
  },
});
