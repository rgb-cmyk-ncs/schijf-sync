# SchijfSync

Keeps folders on two drives the same, live (Windows change notifications for the whole tree,
plus a full comparison at start, on reconnect and every 30 minutes).

- Two ways (both sides equal) or one way (backup: files only on the backup are left alone).
- Removed or overwritten files go to the hidden `.SchijfSync-prullenbak` folder on that drive for 30 days.
- Changed on both sides: the newest wins, the other version is kept in the sync bin.
- Drives are recognised by serial number, so a USB drive under another letter is found again.
- Many removals at once (wrong/empty drive) wait for the user's OK, with a "restore" option.
- Runs in the background (optional start with Windows); the screen is a local page on 127.0.0.1:47125.

Build: push to GitHub; the workflow tests, builds `SchijfSync.exe` and publishes it as release `latest`.
