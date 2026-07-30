# Qoder CLI Auto Auth

Automasi login/logout Qoder CLI dengan Google SSO tanpa perlu buka browser manual.

## Fitur

- ✓ Auto-install Qoder CLI jika belum ada
- ✓ Otomatisasi Google OAuth login
- ✓ Handle semua consent screens (bahasa Indonesia & English)
- ✓ Logout otomatis
- ✓ Mode test (login lalu logout)

## Persyaratan

- **Windows 10/11**
- **Python 3.8+** - [Download](https://www.python.org/downloads/)
- **Node.js** - [Download](https://nodejs.org/) (untuk qodercli)

## Cara Pakai

### 1. Setup (Pertama Kali)

Double-click `setup.bat` atau jalankan:

```cmd
setup.bat
```

Ini akan:
- Install dependencies Python
- Install Playwright browser (Chromium)

### 2. Jalankan

Double-click `run.bat` atau jalankan:

```cmd
run.bat
```

### 3. Ikuti Prompt

Program akan minta:
1. **Google Email** - Email Google kamu
2. **Google Password** - Password Google kamu
3. **Pilihan**:
   - `1` - Login saja
   - `2` - Logout saja
   - `3` - Login lalu logout (test)

## Cara Kerja

```
1. Cek apakah qodercli sudah terinstall
   └─ Jika belum → install via npm

2. Jalankan `qodercli login`
   └─ Capture URL login dari output

3. Buka browser otomatis (Playwright)
   └─ Navigate ke URL login
   └─ Klik "Sign in with Google"
   └─ Input email & password
   └─ Handle consent screens
   └─ Complete OAuth flow

4. qodercli terima token → login selesai!
```

## Environment Variables (Optional)

Bisa set credentials via environment variable:

```cmd
set QODER_GOOGLE_EMAIL=your.email@gmail.com
set QODER_GOOGLE_PASSWORD=yourpassword
run.bat
```

## Troubleshooting

### "Qoder CLI not found"
- Pastikan Node.js terinstall
- Install manual: `npm install -g @qoder-ai/qodercli`

### "Playwright not found"
- Jalankan: `pip install playwright`
- Lalu: `python -m playwright install chromium`

### Login gagal
- Cek email & password benar
- Pastikan tidak ada 2FA (atau handle manual jika ada)
- Coba mode visible (bukan headless) - default sudah visible

### Browser tidak muncul
- Pastikan Playwright terinstall dengan benar
- Jalankan: `python -m playwright install chromium`

## File Structure

```
qoder-bridge/
├── qoder_auto_auth.py    # Script utama
├── requirements.txt      # Python dependencies
├── setup.bat            # Setup script
├── run.bat              # Launcher
└── README.md            # Dokumentasi ini
```

## Catatan Penting

- **Keamanan**: Script ini butuh password Google kamu. Pastikan hanya dijalankan di komputer pribadi.
- **2FA**: Jika akun Google kamu pakai 2FA, mungkin perlu handle manual.
- **Token**: Setelah login sukses, token disimpan di `~/.qoder/.auth/` dan bisa dipakai sampai expired.

## License

MIT - Pakai sesuka hati.
