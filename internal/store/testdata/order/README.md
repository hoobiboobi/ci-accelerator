# Third-party content in these records

The build records in this folder (`*/provenance.json`, `*/record.json`,
`*/logs.bin`) are builds of two open-source projects. Each provenance
carries the project's `Dockerfile` in full, base64-encoded, and the
records and logs name its steps and files:

- `browser-use-1`, `browser-use-2`: builds of browser-use
  (https://github.com/browser-use/browser-use), licensed under the MIT
  License, whose text follows.
- `tradingagents-1`, `tradingagents-2`: builds of TradingAgents
  (https://github.com/TauricResearch/TradingAgents), licensed under the
  Apache License, Version 2.0
  (https://www.apache.org/licenses/LICENSE-2.0, the same licence as this
  repository, see `LICENSE` at its root). The project ships no NOTICE file.

These files are test data only: they are not part of the binary, the
image or the charts.

## browser-use: MIT License

    MIT License

    Copyright (c) 2024 Gregor Zunic

    Permission is hereby granted, free of charge, to any person obtaining a copy
    of this software and associated documentation files (the "Software"), to deal
    in the Software without restriction, including without limitation the rights
    to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
    copies of the Software, and to permit persons to whom the Software is
    furnished to do so, subject to the following conditions:

    The above copyright notice and this permission notice shall be included in all
    copies or substantial portions of the Software.

    THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
    IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
    FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
    AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
    LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
    OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
    SOFTWARE.
