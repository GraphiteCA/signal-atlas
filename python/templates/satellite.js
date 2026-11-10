// Optional authenticated imagery. API keys are tab-memory only, never persisted
// or included in survey exports. Only selecting Satellite starts tile requests.
class SatelliteLayer {
  #key = '';
  constructor(map, onError) { this.map = map; this.layer = null; this.onError = onError; }
  connect(key) { this.#key = key.trim(); if (!this.#key) throw new Error('Enter an ArcGIS API key'); }
  get connected() { return !!this.#key; }
  show() {
    if (!this.connected) return false;
    this.hide();
    this.layer = L.tileLayer('https://ibasemaps-api.arcgis.com/arcgis/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}?token=' + encodeURIComponent(this.#key), {
      maxNativeZoom: 19, maxZoom: 22, updateWhenIdle: true, updateWhenZooming: false, keepBuffer: 1,
      attribution: 'Imagery &copy; Esri, Vantor, Earthstar Geographics, and the GIS User Community'
    });
    if(typeof watchMapTiles==='function')watchMapTiles(this.layer,'Some satellite tiles are unavailable; check the Esri key and connection');
    else this.layer.once('tileerror', () => { this.hide(); this.onError('Satellite imagery unavailable. Check your API key, its basemap privileges, allowed origins and connection.'); });
    this.layer.addTo(this.map);
    return true;
  }
  hide() { if (this.layer) { this.map.removeLayer(this.layer); this.layer = null; } }
  disconnect() { this.hide(); this.#key = ''; }
}
