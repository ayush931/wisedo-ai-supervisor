import React, { useCallback, useEffect, useRef, useState, useMemo } from 'react';
import { Heading } from '@astryxdesign/core/Heading';
import { Button } from '@astryxdesign/core/Button';
import { Table, TableBody, TableCell, TableHeader, TableHeaderCell, TableRow } from '@astryxdesign/core/Table';
import { Announce, Link, Problem, Refreshed, Actor, Time, Val, usePolling, useSession } from './ui';
import { loadLive, loadPlant } from './plant-data';
import { stationState, replayTimes, type LiveStation, type PlantStation } from './plant-model';
import { ApiError } from './api';
import { loadActivity, relatedActivity, sceneMarkers } from './plant-activity';
import { mountPlant, type SceneStation } from './plant-viewport';

function Scene({stations,select,focus}:{stations:readonly SceneStation[];select:(id:string)=>void;focus:readonly string[]|undefined}) {
    const canvas=useRef<HTMLCanvasElement>(null);
    const viewRef=useRef<ReturnType<typeof mountPlant>|null>(null);
    const [failed,setFailed]=useState(false);
    const inventory=stations.map(s=>[s.id,s.line,s.archetype,s.x,s.z].join(':')).join('|');
    useEffect(()=>{
        let view:ReturnType<typeof mountPlant>|undefined;
        const lost=(e:Event)=>{e.preventDefault();setFailed(true);};
        const c=canvas.current!;c.addEventListener('webglcontextlost',lost);
        try{view=mountPlant(c,stations,select);viewRef.current=view;}catch{setFailed(true);}
        return ()=>{viewRef.current=null;c.removeEventListener('webglcontextlost',lost);view?.dispose();};
    },[inventory,select]);
    useEffect(()=>{viewRef.current?.update(stations);},[stations]);
    useEffect(()=>{viewRef.current?.focus(focus);},[focus,inventory]);
    return <>{failed&&<p role="status">3D unavailable. All plant facts remain in the station table.</p>}<canvas hidden={failed} ref={canvas} className="plant-canvas" tabIndex={0} aria-label="Schematic plant. Arrow keys pan, plus and minus zoom, brackets select a station, Enter selects, Home frames the plant. Use the station table for all facts and links." /></>;
}
export default function Plant() {
    const {site}=useSession();
    const [stations,setStations]=useState<LiveStation[]|null>(null),[error,setError]=useState<unknown>(null),[at,setAt]=useState<Date|null>(null),[busy,setBusy]=useState(false);
    const [activity,setActivity]=useState<Awaited<ReturnType<typeof loadActivity>>|null>(null);
    const [replayAt,setReplayAt]=useState<number|null>(null);
    const [cameraFocus,setCameraFocus]=useState<readonly string[]|undefined>();
    const [tableOnly,setTableOnly]=useState(false),[selected,setSelected]=useState('');
    const active=useRef(true),loading=useRef(false),profiles=useRef<PlantStation[]>([]);
    useEffect(()=>{active.current=true;return()=>{active.current=false;};},[]);
    const refresh=useCallback(async()=>{if(loading.current)return;loading.current=true;setBusy(true);try{const [s,a]=await Promise.all([loadPlant(site.id,profiles.current).then(s=>{if(active.current)profiles.current=s;return loadLive(site.id,s);}),loadActivity(site.id,site.role)]);if(active.current){setActivity({...a,items:a.items.map(item=>({...item,stationIDs:item.stationIDs.filter(id=>s.some(st=>st.id===id))}))});setStations(s);setAt(new Date());setError(null);}}catch(e){if(active.current){if(e instanceof ApiError&&[401,403,404].includes(e.problem.status)){setStations(null);setActivity(null);profiles.current=[];}setError(e);}}finally{loading.current=false;if(active.current)setBusy(false);}},[site.id,site.role]);
    usePolling(refresh,30000);
    const times=useMemo(()=>replayTimes(stations??[],at?.getTime()??Date.now()),[stations,at]);
    useEffect(()=>{if(replayAt!==null&&!times.includes(replayAt))setReplayAt(null);},[times,replayAt]);
    const rows=useMemo(()=>stations?.map(s=>({...s,observed:stationState(s,replayAt??at?.getTime()??Date.now())}))??[],[stations,at,replayAt]);
    const scene=useMemo(()=>{const markers=sceneMarkers(activity?.items??[],rows);return rows.map((s,i)=>({...s,color:s.observed.color,person:!!s.observed.person,label:s.name+'\n'+s.observed.status+(s.observed.hold?' · Hold recorded':'')+(s.observed.disposition?'\n'+s.observed.disposition.value+' recorded '+new Date(s.observed.disposition.event_at).toLocaleTimeString():''),...markers[i]}));},[rows,activity]);
    const select=useCallback((id:string)=>{setSelected(id);},[]);
    const replay=<section aria-label="Recorded activity replay" className="plant-replay">
        <Heading level={2}>{replayAt===null?'Live observations':'Recorded activity replay'}</Heading>
        {times.length>0?<><label className="sr-only" htmlFor="plant-time">Recorded event</label><input id="plant-time" type="range" min="0" max={times.length-1} step="1" value={replayAt===null?times.length-1:Math.max(0,times.indexOf(replayAt))} aria-valuetext={new Date(replayAt??times[times.length-1]).toISOString()} onChange={e=>setReplayAt(times[Number(e.target.value)])}/><Time iso={new Date(replayAt??times[times.length-1]).toISOString()}/></>:<p>No current-assignment records to replay.</p>}
    </section>;
    return <section className="plant-page">
        <header className="plant-heading"><div><Heading level={1}>Plant view</Heading><p>Schematic layout · machine shapes are illustrations.</p></div><Refreshed at={at} onRefresh={refresh}/></header>
        <div className="sr-only" role="status" aria-label="Plant refresh"><Announce text={busy?'Refreshing plant…':''}/></div><Problem error={error}/>
        <div className="plant-metrics"><p className="plant-number"><strong>{stations?.length??'—'}</strong> stations</p>{['Running','Stopped','No data'].map(state=><p key={state} className="plant-number"><strong>{stations?rows.filter(s=>s.observed.status===state).length:'—'}</strong> {state==='No data'?state:'recorded '+state.toLowerCase()}</p>)}<p className="plant-number"><strong>{stations?rows.filter(s=>s.observed.hold).length:'—'}</strong> recorded holds</p><p className="plant-number"><strong>{activity&&!activity.errors.some(e=>e.section==='Cases')?activity.items.filter(a=>a.kind==='Case').length:'—'}</strong> open cases</p><p className="plant-number"><strong>{activity&&!activity.errors.some(e=>e.section==='Proposals')?activity.items.filter(a=>a.title==='Awaiting human approval').length:'—'}</strong> human approvals</p></div>
        <div className="plant-controls"><Button label="Whole plant" size="sm" onClick={()=>setCameraFocus([])}/><label className="sr-only" htmlFor="plant-line">Focus line</label><select id="plant-line" value="" onChange={e=>{setCameraFocus(rows.filter(s=>s.line===e.target.value).map(s=>s.id));}}><option value="">Focus a line</option>{[...new Map(rows.map(s=>[s.line,s.lineName])).entries()].map(([id,name])=><option key={id} value={id}>{name}</option>)}</select><Button label="Focus selected station" size="sm" isDisabled={!selected} onClick={()=>selected&&setCameraFocus([selected])}/><Button label={tableOnly?'Show 3D':'Table only'} size="sm" onClick={()=>setTableOnly(v=>!v)}/><Button label="Live observations" size="sm" onClick={()=>setReplayAt(null)}/><Button label="Replay first record" size="sm" isDisabled={!times.length} onClick={()=>setReplayAt(times[0])}/></div>
        <div className={tableOnly?'plant-activity-only':'plant-overview'}>{stations&&!tableOnly&&<div className="plant-stage"><div className="plant-canvas-frame"><Scene stations={scene} select={select} focus={cameraFocus}/><details className="plant-key"><summary>Scene key</summary><p>Strips: green Running · amber Stopped · purple Hold recorded · gray No data / Unknown.</p><p>AI: diamond Case · magnifier Investigation · hourglass Human approval · flag Approved task · pin Correction.</p><p>Line markers appear once at the line head. Orange figures show the last recorder, not live presence.</p></details></div>{replay}</div>}
<aside aria-label="Current AI activity">        <Heading level={2}>AI activity · current</Heading>
        <p>Your accessible activity. Chat: five recent conversations, up to 40 messages each.</p>
        {activity?.errors.map(e=><div key={e.section}><p>{e.section} unavailable</p><Problem error={e.error}/></div>)}
        {activity&&activity.items.length===0&&activity.errors.length===0&&<p>No AI activity recorded.</p>}
        <ul className="plant-feed">{activity?.items.map(a=><li key={`${a.kind}:${a.id}`}><Link to={a.href}>{a.title}</Link> <span>{a.stationIDs.some(id=>rows.some(s=>s.id===id))?rows.filter(s=>a.stationIDs.includes(s.id)).map(s=>s.name).join(", "):a.line?(rows.some(s=>s.line===a.line)?`Line: ${rows.find(s=>s.line===a.line)!.lineName}`:`Line not in this plant view · ${a.line.slice(0,8)}`):"Site scope · no station link"}</span><p>{a.body}</p>{a.time&&<Time iso={a.time}/>}</li>)}</ul></aside></div>
        {tableOnly&&replay}
        <p className="plant-note">Manual observations · refreshes every 30 seconds. Figures show the last recorder, not live presence. Replay uses current profiles and corrected records; AI activity remains current. {error?'Refresh failed; previous observations may be stale.':''}</p>
        {stations?.length===0&&<p>No stations configured.</p>}
        {selected&&<p role="status">Selected: {stations?.find(s=>s.id===selected)?.name}</p>}
        <Table aria-label="Plant stations" density="compact"><TableHeader><TableRow isHeaderRow>{['Station','Line','Illustration / profile','Recorded state','Good / reject','Last recorded by','Assignment','AI activity'].map(h=><TableHeaderCell key={h} scope="col">{h}</TableHeaderCell>)}</TableRow></TableHeader><TableBody>
        {rows.map(s=><TableRow key={s.id}><TableCell><Link to={`/sites/${site.id}/workstations/${s.id}`}>{s.name}</Link><Button label={`Focus ${s.name}`} size="sm" variant="ghost" onClick={()=>{setSelected(s.id);setCameraFocus([s.id]);}}/></TableCell><TableCell>{s.lineName}</TableCell><TableCell>{s.archetype} / {s.template}</TableCell><TableCell>{s.observed.status}{s.observed.hold&&<p>Hold recorded <Time iso={s.observed.holdAt}/></p>}{s.observed.disposition&&<p>{s.observed.disposition.value} recorded <Time iso={s.observed.disposition.event_at}/></p>}{s.observed.reason&&<p>{s.observed.reason}</p>}<Problem error={s.error}/></TableCell><TableCell><Val v={s.observed.good}/> / <Val v={s.observed.reject}/></TableCell><TableCell>{s.observed.person?<Actor a={s.observed.person}/>:"—"}<br/><Time iso={s.observed.event}/></TableCell><TableCell>{s.current?<Link to={`/sites/${site.id}/shifts/${s.current.plan_id}`}>{s.run?.state??"Run unavailable"} · {s.current.order?.order.item_code??s.current.job_order_id}</Link>:s.error?"Unavailable":"No active assignment"}</TableCell><TableCell>{activity?relatedActivity(activity.items,s).map(a=><p key={a.id}><Link to={a.href}>{a.title}</Link>{a.stationIDs.length?" (station scope)":" (line scope)"}</p>):"Unavailable"}{activity&&!activity.errors.length&&!relatedActivity(activity.items,s).length?"No linked activity":""}{activity?.errors.length?" Some AI sources unavailable":""}</TableCell></TableRow>)}
        </TableBody></Table>

    </section>;
}
