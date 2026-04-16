/*
Copyright (C) 2025 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

import { useEffect, useState, useRef } from 'react';
import './home.css';

const terminalLines = [
  { type: 'title', text: 'New Quest: Connect to IINA.AI', icon: '/Dwarf_Scroll_IV.png', delay: 800, showDivider: true },
  { type: 'blank', text: '', delay: 200 },
  { type: 'comment', text: '# Step 1 - Install Claude Code', delay: 600 },
  { type: 'command', text: 'curl -fsSL https://claude.ai/install.sh | bash', delay: 1000 },
  { type: 'output', text: 'Installation complete!', delay: 800 },
  { type: 'blank', text: '', delay: 400 },
  { type: 'comment', text: '# Step 2 - Configure API Gateway', delay: 600 },
  { type: 'command', text: "cat > ~/.claude/settings.json << 'EOF'", delay: 200 },
  { type: 'content', text: '{', delay: 100 },
  { type: 'content', text: '  "env": {', delay: 100 },
  { type: 'content', text: '    "ANTHROPIC_AUTH_TOKEN": "YOUR-KEY",', delay: 100 },
  { type: 'content', text: '    "ANTHROPIC_BASE_URL": "https://iina.ai"', delay: 100 },
  { type: 'content', text: '  }', delay: 100 },
  { type: 'content', text: '}', delay: 100 },
  { type: 'content', text: 'EOF', delay: 800 },
  { type: 'blank', text: '', delay: 400 },
  { type: 'comment', text: '# Step 3 - Start your adventure', delay: 600 },
  { type: 'command', text: 'claude', delay: 800 },
  { type: 'blank', text: '', delay: 300 },
  { type: 'success', text: 'Quest Complete! Happy coding!', delay: 1500 }
];

const CodeTerminal = ({ lines, speed = 35, initialDelay = 500, loop = true, loopDelay = 4000 }) => {
  const [displayLines, setDisplayLines] = useState([]);
  const [showCursor, setShowCursor] = useState(true);
  const containerRef = useRef(null);
  const animationRef = useRef(null);

  useEffect(() => {
    let isMounted = true;

    const scrollToBottom = () => {
      if (containerRef.current) {
        containerRef.current.scrollTop = containerRef.current.scrollHeight;
      }
    };

    const sleep = (ms) => new Promise(resolve => {
      animationRef.current = setTimeout(resolve, ms);
    });

    const runAnimation = async () => {
      while (isMounted) {
        // 重置
        setDisplayLines([]);
        setShowCursor(true);
        await sleep(initialDelay);

        for (let lineIdx = 0; lineIdx < lines.length && isMounted; lineIdx++) {
          const line = lines[lineIdx];
          const isInstant = ['output', 'content', 'blank', 'title', 'success'].includes(line.type);

          if (isInstant) {
            // 瞬间显示
            setDisplayLines(prev => [...prev, {
              type: line.type,
              text: line.text,
              icon: line.icon || null,
              showDivider: line.showDivider || false,
              isComplete: true
            }]);
            scrollToBottom();
            await sleep(line.delay ?? 400);
          } else {
            // 打字效果
            // 先添加空行
            setDisplayLines(prev => [...prev, {
              type: line.type,
              text: '',
              icon: line.icon || null,
              showDivider: line.showDivider || false,
              isComplete: false
            }]);
            scrollToBottom();

            // 逐字显示
            for (let charIdx = 0; charIdx <= line.text.length && isMounted; charIdx++) {
              const currentText = line.text.substring(0, charIdx);
              setDisplayLines(prev => {
                const newLines = [...prev];
                newLines[newLines.length - 1] = {
                  ...newLines[newLines.length - 1],
                  text: currentText,
                  isComplete: charIdx === line.text.length
                };
                return newLines;
              });
              if (charIdx < line.text.length) {
                await sleep(speed);
              }
            }
            await sleep(line.delay ?? 600);
          }
        }

        // 动画完成
        setShowCursor(false);

        if (!loop || !isMounted) break;
        await sleep(loopDelay);
      }
    };

    runAnimation();

    return () => {
      isMounted = false;
      if (animationRef.current) {
        clearTimeout(animationRef.current);
      }
    };
  }, [lines, speed, initialDelay, loop, loopDelay]);

  const getLineClassName = (type) => {
    switch (type) {
      case 'command': return 'text-slate-800';
      case 'comment': return 'text-slate-500 italic';
      case 'output': return 'text-green-700';
      case 'content': return 'text-blue-800';
      case 'title': return 'text-amber-700 font-bold text-xl';
      case 'success': return 'text-green-600 font-bold';
      default: return '';
    }
  };

  const isQuestComplete = displayLines.length > 0 &&
    displayLines[displayLines.length - 1]?.type === 'success' &&
    displayLines[displayLines.length - 1]?.isComplete;

  return (
    <div ref={containerRef} className="paper-content h-[400px] overflow-y-auto p-4 pt-6 text-lg leading-7 text-slate-800 relative scroll-smooth">
      <div className="font-pixel w-full min-h-full pb-8">
        {displayLines.map((line, index) => (
          <div key={index} className="whitespace-pre-wrap break-all">
            {line.type === 'command' && <span className="text-purple-700 font-bold select-none">$ </span>}
            {line.icon && (
              <img
                src={line.icon}
                alt=""
                className="inline-block w-8 h-8 mr-2 align-middle -mt-1"
                style={{ imageRendering: 'pixelated' }}
              />
            )}
            <span className={getLineClassName(line.type)}>{line.text}</span>
            {!line.isComplete && index === displayLines.length - 1 && showCursor && (
              <span className="cursor-blink inline-block w-2 h-5 bg-slate-800 align-middle ml-1"></span>
            )}
            {line.showDivider && line.isComplete && (
              <div className="w-full h-px bg-amber-600/40 mt-2"></div>
            )}
          </div>
        ))}
        {displayLines.length > 0 && !isQuestComplete && showCursor && displayLines[displayLines.length - 1]?.isComplete && (
          <div className="mt-1">
            <span className="text-purple-700 font-bold">$ </span>
            <span className="cursor-blink inline-block w-2 h-5 bg-slate-800 align-middle"></span>
          </div>
        )}
      </div>
    </div>
  );
};

const Home = () => {
  return (
    <div className="sdv-home min-h-screen flex items-center justify-center p-4 pt-20 lg:pt-4">
      <div className="w-full max-w-6xl">
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 items-center">

          {/* 左侧：标题与说明 */}
          <div className="text-center lg:text-left space-y-6 flex flex-col justify-center lg:py-8">
            <div className="inline-block relative">
              <h1 className="font-pixel text-6xl lg:text-7xl leading-none mb-2 logo-text">
                The Unified<br />LLMs API Gateway
              </h1>
              <div className="absolute -top-4 -right-4 text-yellow-400 text-4xl animate-pulse">✦</div>
            </div>

            <p className="font-pixel text-2xl text-sdv-brown-dark lg:pr-12 leading-relaxed">
              The valley's finest connection point.<br />
              <span className="text-sdv-ui-border opacity-80">One interface to rule all models.</span>
            </p>

            {/* 四季树木装饰 */}
            <div className="mt-8 flex justify-center lg:justify-start">
              <img
                src="/trees.png"
                alt="Stardew Valley Trees"
                className="w-72 sm:w-80 lg:w-[400px] h-auto"
                style={{ imageRendering: 'pixelated' }}
              />
            </div>
          </div>

          {/* 右侧：告示板终端 */}
          <div className="terminal-wrapper relative">
            {/* 小鸡装饰 - 告示板左下角外侧 */}
            <div className="hidden lg:block absolute -bottom-4 -left-10 z-30">
              <svg width="36" height="36" viewBox="0 0 16 16" fill="#fff" xmlns="http://www.w3.org/2000/svg" className="drop-shadow-md">
                <path d="M5 2H9V3H10V4H11V6H12V7H13V10H14V12H13V13H11V14H6V13H4V12H3V11H2V10H3V9H2V7H3V6H4V4H5V2ZM5 3V4H4V6H3V7H4V9H5V10H4V11H5V12H6V13H11V12H12V10H13V8H12V7H11V6H10V5H9V3H5Z" fill="#e09c52" />
                <path d="M11 6H12V7H11V6ZM10 4H11V6H10V4Z" fill="#d05040" />
                <path d="M12 8H14V9H13V10H12V8Z" fill="#ffce31" />
                <path d="M7 6H8V7H7V6Z" fill="#333" />
              </svg>
            </div>

            <div className="sdv-panel p-6 rounded-sm">
              {/* 四角木质铆钉 */}
              <div className="wood-corner" style={{ top: '6px', left: '6px' }}></div>
              <div className="wood-corner" style={{ top: '6px', right: '6px' }}></div>
              <div className="wood-corner" style={{ bottom: '6px', left: '6px' }}></div>
              <div className="wood-corner" style={{ bottom: '6px', right: '6px' }}></div>

              <div className="flex justify-between items-center mb-3 border-b-2 border-sdv-ui-border/30 pb-2">
                <span className="font-pixel text-2xl text-sdv-brown-dark uppercase tracking-widest">Notice Board</span>
              </div>

              <div className="relative">
                <div className="quest-board p-1">
                  <CodeTerminal lines={terminalLines} speed={35} loop={true} loopDelay={4000} />
                </div>
              </div>

              <div className="mt-2 text-center">
                <span className="font-pixel text-lg text-sdv-brown-dark opacity-60">- IINA's General Store -</span>
              </div>
            </div>
          </div>

        </div>
      </div>
    </div>
  );
};

export default Home;
